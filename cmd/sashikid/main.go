// sashikid: sashiki デーモン。REST API を提供し、ブランチのライフサイクルを管理する。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rikukaInoue/sashiki/internal/api"
	"github.com/rikukaInoue/sashiki/internal/config"
	"github.com/rikukaInoue/sashiki/internal/engine"
	enginemysql "github.com/rikukaInoue/sashiki/internal/engine/mysql"
	enginepostgres "github.com/rikukaInoue/sashiki/internal/engine/postgres"
	"github.com/rikukaInoue/sashiki/internal/hooks"
	"github.com/rikukaInoue/sashiki/internal/ops"
	"github.com/rikukaInoue/sashiki/internal/proxy"
	"github.com/rikukaInoue/sashiki/internal/state"
	"github.com/rikukaInoue/sashiki/internal/storage"
	storageebszfs "github.com/rikukaInoue/sashiki/internal/storage/ebszfs"
	storagefsxzfs "github.com/rikukaInoue/sashiki/internal/storage/fsxzfs"
	"github.com/rikukaInoue/sashiki/internal/workspace"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsfsxsdk "github.com/aws/aws-sdk-go-v2/service/fsx"
)

var version = "dev" // -ldflags で埋め込む

func main() {
	configPath := flag.String("config", "/etc/sashiki/config.yaml", "path to config.yaml")
	flag.Parse()
	log.Printf("sashikid %s", version)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	db, err := state.Open(cfg.StateDB)
	if err != nil {
		log.Fatalf("state db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var st storage.Storage
	var bp workspace.BaselineProvider
	switch cfg.Storage.Backend {
	case "fsx-zfs":
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(cfg.Storage.Fsx.Region))
		if err != nil {
			log.Fatalf("aws config: %v", err)
		}
		fbe := storagefsxzfs.New(storagefsxzfs.Config{
			FileSystemID:     cfg.Storage.Fsx.FilesystemID,
			BaseVolumeID:     cfg.Storage.Fsx.BaseVolumeID,
			ParentVolumeID:   cfg.Storage.Fsx.ParentVolumeID,
			BaselineSnapshot: cfg.Storage.Fsx.BaselineSnapshot,
			DNSName:          cfg.Storage.Fsx.DNSName,
			MountRoot:        cfg.Storage.Fsx.MountRoot,
		}, awsfsxsdk.NewFromConfig(awsCfg))
		st, bp = fbe, fbe
	default:
		zbe := storageebszfs.New(storageebszfs.Config{
			Pool:             cfg.Storage.Zfs.Pool,
			BaseDataset:      cfg.Storage.Zfs.BaseDataset,
			BranchParent:     cfg.Storage.Zfs.BranchParent,
			BaselineSnapshot: cfg.Storage.Zfs.BaselineSnapshot,
			Sudo:             cfg.Storage.Zfs.Sudo,
		})
		st, bp = zbe, zbe
	}

	var eng engine.Engine
	switch cfg.Engine.Type {
	case "postgres":
		eng = enginepostgres.New(enginepostgres.Config{
			EnvDir:          cfg.Engine.Postgres.EnvDir,
			BinDir:          cfg.Engine.Postgres.BinDir,
			ListenAddresses: cfg.Engine.Postgres.ListenAddresses,
			Sudo:            cfg.Engine.Postgres.Sudo,
		})
		// postgres はプロキシを通らないため last_conn_at が更新されず、
		// リーパーが使用中ブランチを「アイドル」と誤判定して停止・削除してしまう。
		// 接続追跡ができるようになるまでアイドル回収は無効化する。
		if cfg.Branches.IdleStopAfter > 0 || cfg.Branches.DeleteAfterIdle > 0 {
			log.Printf("sashikid: engine=postgres では接続追跡ができないため idle_stop_after / delete_after_idle を無効化します")
			cfg.Branches.IdleStopAfter = 0
			cfg.Branches.DeleteAfterIdle = 0
		}
	default:
		eng = enginemysql.New(enginemysql.Config{
			EnvDir:    cfg.Engine.Mysql.EnvDir,
			ProxyUser: cfg.Engine.Mysql.ProxyUser,
			ProxyPass: cfg.Engine.Mysql.ProxyPass,
			Sudo:      cfg.Engine.Mysql.Sudo,
		})
	}

	hr := hooks.NewRunner(cfg.Hooks.Dir, cfg.Hooks.LogDir, cfg.Hooks.Timeout)

	mgr, err := workspace.New(workspace.Config{
		NamePattern:         cfg.Branches.NamePattern,
		MaxBranches:         cfg.Branches.MaxBranches,
		PortLow:             cfg.PortRange()[0],
		PortHigh:            cfg.PortRange()[1],
		EngineType:          cfg.Engine.Type,
		StateDir:            "/var/lib/sashiki/branches",
		LazyCreate:          cfg.Branches.LazyCreate,
		LazyMaxWait:         cfg.Branches.LazyCreateMaxWait,
		IdleStopAfter:       cfg.Branches.IdleStopAfter,
		DeleteAfterIdle:     cfg.Branches.DeleteAfterIdle,
		AvailableMem:        availableMem,
		ExpectedRSSBytes:    parseSize(cfg.Engine.Mysql.ExpectedRSS),
		MemoryHeadroomBytes: parseSize(cfg.Engine.Mysql.MemoryHeadroom),
		BufferPoolBytes:     parseSize(cfg.Engine.Mysql.BufferPoolSize),
		MaxRunning:          cfg.Engine.Mysql.MaxRunning,
		HighWatermark:       cfg.Storage.HighWatermark,
		CriticalWatermark:   cfg.Storage.CriticalWatermark,
	}, st, bp, eng, hr, db)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}

	token := os.Getenv(cfg.Auth.APITokenEnv)
	srv := api.New(mgr, cfg.Domain, cfg.Engine.Type, cfg.Engine.Mysql.ProxyUser, cfg.Engine.Mysql.ProxyPass, token, db)
	srv.SetOps(ops.New(db))
	mgr.SetBaselinePolicy(workspace.RefreshConfig{
		RequireMasked:    cfg.Baseline.RequireMasked,
		RequireValidated: cfg.Baseline.RequireValidated,
		ValidatePort:     cfg.Baseline.ValidatePort,
		MaskedSentinel:   cfg.Baseline.MaskedSentinel,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 起動時に state.db と ZFS/engine を突き合わせる(仕様 20-1)。
	if rep, err := mgr.Reconcile(context.Background()); err != nil {
		log.Printf("reconcile: %v", err)
	} else if len(rep.Demoted)+len(rep.Errored)+len(rep.Orphans) > 0 {
		log.Printf("reconcile: demoted=%d errored=%d orphans=%d",
			len(rep.Demoted), len(rep.Errored), len(rep.Orphans))
	}

	go mgr.RunReaper(ctx, cfg.Branches.ReaperInterval)

	if cfg.Listen.Metrics != "" {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("GET /metrics", api.MetricsHandler(mgr))
			srv := &http.Server{Addr: cfg.Listen.Metrics, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			log.Printf("sashikid: metrics listening on %s", cfg.Listen.Metrics)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("metrics: %v", err)
			}
		}()
	}

	if cfg.Listen.Proxy != "" && cfg.Engine.Type != "mysql" {
		log.Printf("sashikid: プロキシは MySQL 専用のため engine=%s では起動しません(警告を消すには listen.proxy: \"\")", cfg.Engine.Type)
	}
	if cfg.Listen.Proxy != "" && cfg.Engine.Type == "mysql" {
		px, err := proxy.New(proxy.Config{
			Listen:           cfg.Listen.Proxy,
			NamePattern:      cfg.Branches.NamePattern,
			MaxConnPerBranch: cfg.Proxy.MaxConnPerBranch,
			AllowedUser:      cfg.Engine.Mysql.ProxyUser,
		}, mgr)
		if err != nil {
			log.Fatalf("proxy: %v", err)
		}
		mgr.SetActiveConns(px.ActiveConns)
		go func() {
			if err := px.Listen(ctx); err != nil {
				log.Fatalf("proxy: %v", err)
			}
		}()
		log.Printf("sashikid: proxy listening on %s", cfg.Listen.Proxy)
	}

	log.Printf("sashikid: listening on %s (backend=%s engine=%s)",
		cfg.Listen.API, cfg.Storage.Backend, cfg.Engine.Type)
	if err := srv.Listen(ctx, cfg.Listen.API); err != nil {
		log.Fatal(err)
	}
}
