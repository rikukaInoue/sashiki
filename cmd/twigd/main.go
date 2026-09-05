// twigd: twig デーモン。REST API を提供し、ブランチのライフサイクルを管理する。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/rikukaInoue/twig/internal/api"
	"github.com/rikukaInoue/twig/internal/branch"
	"github.com/rikukaInoue/twig/internal/config"
	enginemysql "github.com/rikukaInoue/twig/internal/engine/mysql"
	"github.com/rikukaInoue/twig/internal/hooks"
	"github.com/rikukaInoue/twig/internal/proxy"
	"github.com/rikukaInoue/twig/internal/state"
	storagezfs "github.com/rikukaInoue/twig/internal/storage/zfs"
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

	zbe := storagezfs.New(storagezfs.Config{
		Pool:             cfg.Storage.Zfs.Pool,
		BaseDataset:      cfg.Storage.Zfs.BaseDataset,
		BranchParent:     cfg.Storage.Zfs.BranchParent,
		BaselineSnapshot: cfg.Storage.Zfs.BaselineSnapshot,
		Sudo:             cfg.Storage.Zfs.Sudo,
	})

	eng := enginemysql.New(enginemysql.Config{
		EnvDir:    cfg.Engine.Mysql.EnvDir,
		ProxyUser: cfg.Engine.Mysql.ProxyUser,
		ProxyPass: cfg.Engine.Mysql.ProxyPass,
		Sudo:      cfg.Engine.Mysql.Sudo,
	})

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
	}, zbe, zbe, eng, hr, db)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}

	token := os.Getenv(cfg.Auth.APITokenEnv)
	srv := api.New(mgr, cfg.Domain, cfg.Engine.Mysql.ProxyUser, token, db)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go mgr.RunReaper(ctx, cfg.Branches.ReaperInterval)

	if cfg.Listen.Proxy != "" {
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
