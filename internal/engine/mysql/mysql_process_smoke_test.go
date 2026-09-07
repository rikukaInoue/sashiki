package mysql

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rikukaInoue/sashiki/internal/engine"
)

// TestProcessRealMysqld は実 mysqld で process モードのライフサイクルを検証する。
// mysqld が無い環境(CI の Linux ジョブ等)では Skip する。auth 設定に依存せず、
// 「起動して port が開く → graceful stop で消える」までを見る。
func TestProcessRealMysqld(t *testing.T) {
	bin, err := exec.LookPath("mysqld")
	if err != nil {
		t.Skip("mysqld not installed; skipping process-mode smoke test")
	}
	datadir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(datadir, 0o755); err != nil {
		t.Fatal(err)
	}
	// datadir を初期化(root はパスワード無し)。
	initCmd := exec.Command(bin, "--no-defaults", "--initialize-insecure", "--datadir="+datadir)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Skipf("mysqld --initialize failed in this environment: %v: %s", err, out)
	}

	port := freePort(t)
	e := New(Config{Mode: ModeProcess, MysqldBin: bin, ReadyTimeout: 30 * time.Second})
	ins := engine.Instance{Branch: "smoke", DataDir: datadir, Port: port}

	if err := e.Start(context.Background(), ins); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = e.Kill(context.Background(), ins) }()

	if ok, _ := e.IsRunning(context.Background(), ins); !ok {
		t.Fatal("IsRunning should be true after Start")
	}
	if !waitPort(port, 30*time.Second) {
		t.Fatalf("mysqld did not open port %d", port)
	}

	// graceful stop → 消える & port 閉じる
	if err := e.Stop(context.Background(), ins); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if ok, _ := e.IsRunning(context.Background(), ins); ok {
		t.Error("IsRunning should be false after Stop")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitPort(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("127.0.0.1", itoa(port))
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
