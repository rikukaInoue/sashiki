// Package mysql は systemd テンプレートユニット(mysqld@<branch>)で
// ブランチごとの mysqld を管理する engine.Engine 実装。
// ポート等は /etc/sashiki/<branch>.env に書き、ユニットが EnvironmentFile で読む。
package mysql

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

// Config は mysql エンジンの設定。
type Config struct {
	EnvDir       string // /etc/sashiki
	UnitTemplate string // 既定 "mysqld"→ mysqld@<branch>.service
	ProxyUser    string // ready 判定に使う接続ユーザー(既定 dev)
	ProxyPass    string
	ReadyTimeout time.Duration // 既定 30s
	Sudo         bool

	// Mode は起動方式。"systemd"(既定)は systemd テンプレートユニット、
	// "process" は mysqld を直接 spawn する(systemd の無い macOS ネイティブ /
	// コンテナ向け、#113)。
	Mode string
	// MysqldBin は process モードで起動する mysqld のパス(既定 "mysqld")。
	MysqldBin string
	// RunUser は root で動かすとき mysqld に渡す --user(既定 "mysql")。
	// root でなければ無視する。
	RunUser string
}

const (
	ModeSystemd = "systemd"
	ModeProcess = "process"
)

// Engine は engine.Engine の MySQL + systemd 実装。
type Engine struct {
	cfg Config
	run func(ctx context.Context, name string, args ...string) (string, error)
}

// New は MySQL エンジンを作る。
func New(cfg Config) *Engine {
	if cfg.UnitTemplate == "" {
		cfg.UnitTemplate = "mysqld"
	}
	if cfg.ProxyUser == "" {
		cfg.ProxyUser = "dev"
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeSystemd
	}
	if cfg.MysqldBin == "" {
		cfg.MysqldBin = "mysqld"
	}
	if cfg.RunUser == "" {
		cfg.RunUser = "mysql"
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

func (e *Engine) envPath(branch string) string {
	return filepath.Join(e.cfg.EnvDir, branch+".env")
}

// Start はインスタンスを起動する(mode により systemd / 直 spawn)。
func (e *Engine) Start(ctx context.Context, ins engine.Instance) error {
	if e.cfg.Mode == ModeProcess {
		return e.startProcess(ctx, ins)
	}
	env := fmt.Sprintf("PORT=%d\nDATADIR=%s\n", ins.Port, ins.DataDir)
	if err := os.WriteFile(e.envPath(ins.Branch), []byte(env), 0o644); err != nil {
		return fmt.Errorf("write env: %w", err)
	}
	_, err := e.run(ctx, "systemctl", "start", e.unit(ins.Branch))
	return err
}

// Stop は正常終了(graceful)させる。snapshot の一貫性はこれに依存する。
func (e *Engine) Stop(ctx context.Context, ins engine.Instance) error {
	if e.cfg.Mode == ModeProcess {
		return e.stopProcess(ctx, ins)
	}
	if _, err := e.run(ctx, "systemctl", "stop", e.unit(ins.Branch)); err != nil {
		return err
	}
	// env は消してよい(再 Start 時に書き直す)。失敗しても致命ではない。
	_ = os.Remove(e.envPath(ins.Branch))
	return nil
}

// Kill は即時停止(dirty state を捨てる。rollback 直前用、snapshot 前には使わない)。
func (e *Engine) Kill(ctx context.Context, ins engine.Instance) error {
	if e.cfg.Mode == ModeProcess {
		return e.killProcess(ctx, ins)
	}
	_, _ = e.run(ctx, "systemctl", "kill", "-s", "SIGKILL", e.unit(ins.Branch))
	// プロセス消滅を待たずとも rollback は volume を置き換えるが、
	// datadir を掴んだままの rollback を避けるため軽く待つ。
	_, _ = e.run(ctx, "systemctl", "stop", e.unit(ins.Branch))
	_ = os.Remove(e.envPath(ins.Branch))
	return nil
}

// WaitReady は mysqladmin ping が通るまで 100ms 間隔で待つ。
func (e *Engine) WaitReady(ctx context.Context, ins engine.Instance) error {
	deadline := time.Now().Add(e.cfg.ReadyTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "mysqladmin",
			"-u"+e.cfg.ProxyUser, "-p"+e.cfg.ProxyPass,
			"-h127.0.0.1", fmt.Sprintf("-P%d", ins.Port), "ping")
		if err := cmd.Run(); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout: mysqld@%s (port %d) did not become ready in %s",
		ins.Branch, ins.Port, e.cfg.ReadyTimeout)
}

// IsRunning は起動中かどうか(mode により systemctl / pidfile)。
func (e *Engine) IsRunning(ctx context.Context, ins engine.Instance) (bool, error) {
	if e.cfg.Mode == ModeProcess {
		return e.isRunningProcess(ins), nil
	}
	out, _ := e.run(ctx, "systemctl", "is-active", e.unit(ins.Branch))
	return out == "active", nil
}

// ConnCount は現在のクライアント接続数を返す(engine.ConnCounter, #41)。
// `SHOW STATUS LIKE 'Threads_connected'` を mysql CLI で取得し、自分(この
// クライアント)の接続を1つ差し引く。sudo は不要(TCP で dev ユーザー接続)。
func (e *Engine) ConnCount(ctx context.Context, ins engine.Instance) (int, error) {
	cmd := exec.CommandContext(ctx, "mysql",
		"-u"+e.cfg.ProxyUser, "-p"+e.cfg.ProxyPass,
		"-h127.0.0.1", fmt.Sprintf("-P%d", ins.Port),
		"-N", "-B", "-e", "SHOW STATUS LIKE 'Threads_connected'")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("mysql show status (port %d): %w", ins.Port, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected SHOW STATUS output: %q", strings.TrimSpace(string(out)))
	}
	n, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		return 0, fmt.Errorf("parse threads_connected %q: %w", fields[len(fields)-1], err)
	}
	if n > 0 {
		n-- // 自分の接続を除く
	}
	return n, nil
}
