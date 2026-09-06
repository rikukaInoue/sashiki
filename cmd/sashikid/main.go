// sashikid: sashiki デーモン。REST API を提供し、ブランチのライフサイクルを管理する。
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
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

	// 構造化ログ(仕様 20-5)。json では既存の log.Printf も slog 経由で JSON になる
	if cfg.LogFormat == "json" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
		log.SetFlags(0)
		log.SetOutput(slogWriter{})
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
		// postgres の idle 回収は #41 の capability 判定(下)に一本化した。
		// postgres は ConnCounter を実装しているので connpoll が last_conn_at を
		// 更新でき、idle_stop_after / delete_after_idle / profile を無効化する必要はない。
	default:
		eng = enginemysql.New(enginemysql.Config{
			EnvDir:    cfg.Engine.Mysql.EnvDir,
			ProxyUser: cfg.Engine.Mysql.ProxyUser,
			ProxyPass: cfg.Engine.Mysql.ProxyPass,
			Sudo:      cfg.Engine.Mysql.Sudo,
		})
	}

	// idle 回収は「接続の有無が分かる」ことが前提。engine が接続数を取得できる
	// (engine.ConnCounter)なら connpoll が last_conn_at を更新するので、proxy を
	// 通らない postgres / fsx 直続でも安全に回収できる(#41: 旧 postgres 無効化の解除)。
	// 取得手段が無い engine のときだけ idle_stop_after / delete_after_idle を無効化する。
	if _, ok := eng.(engine.ConnCounter); !ok {
		if cfg.Branches.IdleStopAfter > 0 || cfg.Branches.DeleteAfterIdle > 0 {
			log.Printf("sashikid: engine=%s は接続数を取得できない(ConnCounter 未実装)ため idle_stop_after / delete_after_idle を無効化します", cfg.Engine.Type)
			cfg.Branches.IdleStopAfter = 0
			cfg.Branches.DeleteAfterIdle = 0
		}
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
		Profiles:            profilePolicies(cfg.Branches.Profiles),
		DefaultProfile:      cfg.Branches.DefaultProfile,
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
	// engine を定期ポーリングして last_conn_at を更新する(#41)。engine が
	// ConnCounter 未実装なら内部で即 return する。
	go mgr.RunConnPoller(ctx, cfg.Branches.ReaperInterval)

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

// slogWriter は既存の log.Printf 出力を slog(JSON)へ橋渡しする。
type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	slog.Info(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// profilePolicies は config の profile 定義を workspace の型へ変換する(#34)。
func profilePolicies(in map[string]config.ProfilePolicy) map[string]workspace.ProfilePolicy {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]workspace.ProfilePolicy, len(in))
	for name, p := range in {
		out[name] = workspace.ProfilePolicy{
			IdleStopAfter:   p.IdleStopAfter,
			DeleteAfterIdle: p.DeleteAfterIdle,
		}
	}
	return out
}
