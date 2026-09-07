package mysql

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rikukaInoue/sashiki/internal/engine"
)

func TestNewDefaultsMode(t *testing.T) {
	e := New(Config{})
	if e.cfg.Mode != ModeSystemd {
		t.Errorf("default mode = %q, want systemd", e.cfg.Mode)
	}
	if e.cfg.MysqldBin != "mysqld" {
		t.Errorf("default MysqldBin = %q", e.cfg.MysqldBin)
	}
}

func TestStartArgs(t *testing.T) {
	e := New(Config{Mode: ModeProcess})
	ins := engine.Instance{Branch: "pr-1", DataDir: "/data/pr-1", Port: 3401}
	args := strings.Join(e.startArgs(ins), " ")
	for _, want := range []string{
		"--no-defaults",
		"--datadir=/data/pr-1",
		"--port=3401",
		"--socket=/tmp/sashiki-3401.sock",
		"--pid-file=/data/pr-1/mysqld.pid",
		"--daemonize",
		"--mysqlx=OFF",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("startArgs missing %q; got: %s", want, args)
		}
	}
}

func TestStartArgsExtraCnf(t *testing.T) {
	e := New(Config{Mode: ModeProcess, ExtraCnf: "/etc/sashiki/server.80.cnf"})
	ins := engine.Instance{Branch: "pr-1", DataDir: "/data/pr-1", Port: 3401}
	args := e.startArgs(ins)
	if args[0] != "--defaults-extra-file=/etc/sashiki/server.80.cnf" {
		t.Errorf("args[0] = %q, want --defaults-extra-file=...", args[0])
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--no-defaults") {
		t.Error("--no-defaults must not coexist with --defaults-extra-file")
	}
	// sashiki が制御する項目は後続で必ず渡る(cnf を上書きできる)
	for _, want := range []string{"--datadir=/data/pr-1", "--port=3401", "--pid-file=/data/pr-1/mysqld.pid"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// spawnFake は SIGTERM/SIGKILL で死ぬ実プロセスを起動し、その pid を pidfile に書く。
// mysqld の代わりにライフサイクル制御(pidfile + シグナル)を検証するためのもの。
func spawnFake(t *testing.T, e *Engine, ins engine.Instance) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(ins.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }() // 反リープ: 死んだら回収してゾンビ化を防ぐ
	pidfile := e.pidPath(ins)
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestProcessLifecycleKill(t *testing.T) {
	e := New(Config{Mode: ModeProcess, ReadyTimeout: 3 * time.Second})
	ins := engine.Instance{Branch: "pr-1", DataDir: filepath.Join(t.TempDir(), "pr-1"), Port: 3401}

	// 起動前: 動いていない
	if e.isRunningProcess(ins) {
		t.Fatal("should not be running before start")
	}
	cmd := spawnFake(t, e, ins)
	defer func() { _ = cmd.Process.Kill() }()

	if !e.isRunningProcess(ins) {
		t.Fatal("should be running after pidfile written")
	}
	if err := e.Kill(context.Background(), ins); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if e.isRunningProcess(ins) {
		t.Error("should be gone after Kill")
	}
	if _, err := os.Stat(e.pidPath(ins)); !os.IsNotExist(err) {
		t.Error("Kill should remove the pidfile")
	}
}

func TestProcessGracefulStop(t *testing.T) {
	e := New(Config{Mode: ModeProcess, ReadyTimeout: 3 * time.Second})
	ins := engine.Instance{Branch: "pr-2", DataDir: filepath.Join(t.TempDir(), "pr-2"), Port: 3402}
	cmd := spawnFake(t, e, ins)
	defer func() { _ = cmd.Process.Kill() }()

	// Stop は SIGTERM → sleep は終了する
	if err := e.Stop(context.Background(), ins); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if e.isRunningProcess(ins) {
		t.Error("should be stopped after graceful Stop")
	}
}

func TestStopWhenAlreadyGone(t *testing.T) {
	e := New(Config{Mode: ModeProcess, ReadyTimeout: time.Second})
	ins := engine.Instance{Branch: "pr-3", DataDir: filepath.Join(t.TempDir(), "pr-3"), Port: 3403}
	// pidfile が無い(既に停止済み)場合は no-op で成功する
	if err := e.Stop(context.Background(), ins); err != nil {
		t.Errorf("Stop on already-gone instance should succeed, got %v", err)
	}
}
