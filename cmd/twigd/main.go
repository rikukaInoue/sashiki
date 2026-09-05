// twigd: twig デーモン。REST API を提供し、ブランチのライフサイクルを管理する。
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

	"github.com/rikukaInoue/twig/internal/api"
	"github.com/rikukaInoue/twig/internal/branch"
	"github.com/rikukaInoue/twig/internal/config"
	"github.com/rikukaInoue/twig/internal/engine"
	enginemysql "github.com/rikukaInoue/twig/internal/engine/mysql"
	enginepostgres "github.com/rikukaInoue/twig/internal/engine/postgres"
	"github.com/rikukaInoue/twig/internal/hooks"
	"github.com/rikukaInoue/twig/internal/proxy"
	"github.com/rikukaInoue/twig/internal/state"
	"github.com/rikukaInoue/twig/internal/storage"
	storagefsx "github.com/rikukaInoue/twig/internal/storage/fsx"
	storagezfs "github.com/rikukaInoue/twig/internal/storage/zfs"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsfsxsdk "github.com/aws/aws-sdk-go-v2/service/fsx"
)

var version = "dev" // -ldflags で埋め込む

func main() {
	configPath := flag.String("config", "/etc/twig/config.yaml", "path to config.yaml")
	flag.Parse()
	log.Printf("twigd %s", version)

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
	var bp branch.BaselineProvider
	switch cfg.Storage.Backend {
	case "fsx":
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(cfg.Storage.Fsx.Region))
		if err != nil {
			log.Fatalf("aws config: %v", err)
		}
		fbe := storagefsx.New(storagefsx.Config{
			FileSystemID:     cfg.Storage.Fsx.FilesystemID,
			BaseVolumeID:     cfg.Storage.Fsx.BaseVolumeID,
			ParentVolumeID:   cfg.Storage.Fsx.ParentVolumeID,
			BaselineSnapshot: cfg.Storage.Fsx.BaselineSnapshot,
			DNSName:          cfg.Storage.Fsx.DNSName,
			MountRoot:        cfg.Storage.Fsx.MountRoot,
		}, awsfsxsdk.NewFromConfig(awsCfg))
		st, bp = fbe, fbe
	default:
		zbe := storagezfs.New(storagezfs.Config{
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
		envDir := cfg.Engine.Postgres.EnvDir
		if envDir == "" {
			envDir = cfg.Engine.Mysql.EnvDir
		}
		eng = enginepostgres.New(enginepostgres.Config{
			EnvDir: envDir,
			BinDir: cfg.Engine.Postgres.BinDir,
			Sudo:   cfg.Engine.Postgres.Sudo,
		})
	default:
		eng = enginemysql.New(enginemysql.Config{
			EnvDir:    cfg.Engine.Mysql.EnvDir,
			ProxyUser: cfg.Engine.Mysql.ProxyUser,
			ProxyPass: cfg.Engine.Mysql.ProxyPass,
			Sudo:      cfg.Engine.Mysql.Sudo,
		})
	}

	hr := hooks.NewRunner(cfg.Hooks.Dir, cfg.Hooks.LogDir, cfg.Hooks.Timeout)

	mgr, err := branch.New(branch.Config{
		NamePattern:     cfg.Branches.NamePattern,
		MaxBranches:     cfg.Branches.MaxBranches,
		PortLow:         cfg.Engine.Mysql.PortRange[0],
		PortHigh:        cfg.Engine.Mysql.PortRange[1],
		EngineType:      cfg.Engine.Type,
		StateDir:        "/var/lib/twig/branches",
		LazyCreate:      cfg.Branches.LazyCreate,
		LazyMaxWait:     cfg.Branches.LazyCreateMaxWait,
		IdleStopAfter:   cfg.Branches.IdleStopAfter,
		DeleteAfterIdle: cfg.Branches.DeleteAfterIdle,
		AvailableMem:    availableMem,
		BufferPoolBytes: parseSize(cfg.Engine.Mysql.BufferPoolSize),
	}, st, bp, eng, hr, db)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}

	token := os.Getenv(cfg.Auth.APITokenEnv)
	srv := api.New(mgr, cfg.Domain, cfg.Engine.Mysql.ProxyUser, token, db)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go mgr.RunReaper(ctx, cfg.Branches.ReaperInterval)

	if cfg.Listen.Metrics != "" {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("GET /metrics", api.MetricsHandler(mgr))
			srv := &http.Server{Addr: cfg.Listen.Metrics, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			log.Printf("twigd: metrics listening on %s", cfg.Listen.Metrics)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("metrics: %v", err)
			}
		}()
	}

	if cfg.Listen.Proxy != "" && cfg.Engine.Type == "mysql" {
		px, err := proxy.New(proxy.Config{
			Listen:           cfg.Listen.Proxy,
			NamePattern:      cfg.Branches.NamePattern,
			MaxConnPerBranch: cfg.Proxy.MaxConnPerBranch,
		}, mgr)
		if err != nil {
			log.Fatalf("proxy: %v", err)
		}
		go func() {
			if err := px.Listen(ctx); err != nil {
				log.Fatalf("proxy: %v", err)
			}
		}()
		log.Printf("twigd: proxy listening on %s", cfg.Listen.Proxy)
	}

	log.Printf("twigd: listening on %s (backend=%s engine=%s)",
		cfg.Listen.API, cfg.Storage.Backend, cfg.Engine.Type)
	if err := srv.Listen(ctx, cfg.Listen.API); err != nil {
		log.Fatal(err)
	}
}
