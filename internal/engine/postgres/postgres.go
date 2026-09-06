// Package postgres は PostgreSQL の engine.Engine 実装。
// systemd テンプレートユニット(postgres-sashiki@<branch>)でブランチごとの
// postgres を起動する。MySQL プロトコルプロキシは使えないため、接続は
// 直接ポート(sashiki show <name> で確認)になる(既知の制限)。
//
// リモート接続する場合は engine.postgres.listen_addresses を "*" 等に広げ、
// かつ base の pg_hba.conf にクライアント側ネットワークの host 行が必要
// (pg_hba はブランチにクローンされるので base に入れておく)。
//
// storage 側の注意: Postgres のページサイズは 8KB のため、base データセットと
// branch_parent(クローンは名前空間上の親からプロパティを継承する)は
// recordsize=8k で作るのが望ましい(zfs backend の設定で変更可能)。
package postgres

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rikukaInoue/sashiki/internal/engine"
)

// Config は postgres エンジンの設定。
type Config struct {
	EnvDir          string // /etc/sashiki
	UnitTemplate    string // 既定 "postgres-sashiki" → postgres-sashiki@<branch>.service
	BinDir          string // 既定 /usr/lib/postgresql/16/bin
	ListenAddresses string // 既定 127.0.0.1。リモート接続を許すなら "*" 等
	ReadyTimeout    time.Duration
	Sudo            bool
}

// Engine は engine.Engine の PostgreSQL + systemd 実装。
type Engine struct {
	cfg Config
	run func(ctx context.Context, name string, args ...string) (string, error)
}

// New は PostgreSQL エンジンを作る。
func New(cfg Config) *Engine {
	if cfg.UnitTemplate == "" {
		cfg.UnitTemplate = "postgres-sashiki"
	}
	if cfg.BinDir == "" {
		cfg.BinDir = "/usr/lib/postgresql/16/bin"
	}
	if cfg.ListenAddresses == "" {
		cfg.ListenAddresses = "127.0.0.1"
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	e := &Engine{cfg: cfg}
	e.run = e.execCmd
	return e
}

func (e *Engine) execCmd(ctx context.Context, name string, args ...string) (string, error) {
	var cmd *exec.Cmd
	if e.cfg.Sudo {
		cmd = exec.CommandContext(ctx, "sudo", append([]string{"-n", name}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (e *Engine) unit(branch string) string {
	return fmt.Sprintf("%s@%s", e.cfg.UnitTemplate, branch)
}

// Start は env ファイルを書いて systemd ユニットを起動する。
func (e *Engine) Start(ctx context.Context, ins engine.Instance) error {
	env := fmt.Sprintf("PORT=%d\nDATADIR=%s\nPGBIN=%s\nLISTEN_ADDRESSES=%s\n",
		ins.Port, ins.DataDir, e.cfg.BinDir, e.cfg.ListenAddresses)
	if err := os.WriteFile(filepath.Join(e.cfg.EnvDir, ins.Branch+".env"), []byte(env), 0o644); err != nil {
		return fmt.Errorf("write env: %w", err)
	}
	_, err := e.run(ctx, "systemctl", "start", e.unit(ins.Branch))
	return err
}

// Stop は systemd 経由で停止する(ユニット側で pg_ctl の fast shutdown を使う)。
func (e *Engine) Stop(ctx context.Context, ins engine.Instance) error {
	if _, err := e.run(ctx, "systemctl", "stop", e.unit(ins.Branch)); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(e.cfg.EnvDir, ins.Branch+".env"))
	return nil
}

// Kill は即時停止(immediate)。dirty state を捨てる rollback 用。
func (e *Engine) Kill(ctx context.Context, ins engine.Instance) error {
	_, _ = e.run(ctx, "systemctl", "kill", "-s", "SIGKILL", e.unit(ins.Branch))
	_, _ = e.run(ctx, "systemctl", "stop", e.unit(ins.Branch))
	_ = os.Remove(filepath.Join(e.cfg.EnvDir, ins.Branch+".env"))
	return nil
}

// WaitReady は pg_isready が通るまで待つ。
func (e *Engine) WaitReady(ctx context.Context, ins engine.Instance) error {
	deadline := time.Now().Add(e.cfg.ReadyTimeout)
	bin := filepath.Join(e.cfg.BinDir, "pg_isready")
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, bin, "-h", "127.0.0.1", "-p", fmt.Sprintf("%d", ins.Port))
		if err := cmd.Run(); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout: postgres@%s (port %d) did not become ready in %s",
		ins.Branch, ins.Port, e.cfg.ReadyTimeout)
}

// IsRunning は systemctl is-active で判定する。
func (e *Engine) IsRunning(ctx context.Context, ins engine.Instance) (bool, error) {
	out, _ := e.run(ctx, "systemctl", "is-active", e.unit(ins.Branch))
	return out == "active", nil
}

// ConnCount は現在のクライアント接続数を返す(engine.ConnCounter, #41)。
// pg_stat_activity から client backend を数え、自分(このポーラ)の接続は除く。
// 127.0.0.1 への接続が pg_hba で許可されている前提(base の pg_hba.conf に
// host 行を入れておく。パッケージ doc 参照)。認証情報を持たないため -w で
// 即失敗させ、失敗時はポーラ側で「判定不能=使用中」として保護する。
func (e *Engine) ConnCount(ctx context.Context, ins engine.Instance) (int, error) {
	psql := filepath.Join(e.cfg.BinDir, "psql")
	cmd := exec.CommandContext(ctx, psql,
		"-h", "127.0.0.1", "-p", strconv.Itoa(ins.Port),
		"-U", "postgres", "-d", "postgres", "-w", "-tAc",
		"SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("psql pg_stat_activity (port %d): %w", ins.Port, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("parse count %q: %w", strings.TrimSpace(string(out)), err)
	}
	return n, nil
}
