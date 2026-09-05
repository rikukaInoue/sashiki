package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunNoHookIsNoop(t *testing.T) {
	r := NewRunner(t.TempDir(), t.TempDir(), time.Minute)
	res, err := r.Run(context.Background(), OnCreate, Env{Branch: "pr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ran {
		t.Error("Ran = true, want false")
	}
}

func TestRunPassesEnvAndLogs(t *testing.T) {
	dir, logDir := t.TempDir(), t.TempDir()
	writeScript(t, dir, "on-create.sh", "#!/bin/sh\necho \"branch=$SASHIKI_BRANCH port=$SASHIKI_PORT event=$SASHIKI_EVENT\"\n")
	r := NewRunner(dir, logDir, time.Minute)

	res, err := r.Run(context.Background(), OnCreate, Env{Branch: "pr-1", Port: 3401})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ran || res.ExitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	log, err := os.ReadFile(res.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(log))
	if got != "branch=pr-1 port=3401 event=on-create" {
		t.Errorf("log = %q", got)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "on-create", "#!/bin/sh\nexit 3\n") // 拡張子なしでも拾う
	r := NewRunner(dir, t.TempDir(), time.Minute)

	res, err := r.Run(context.Background(), OnCreate, Env{Branch: "pr-1"})
	if err == nil {
		t.Fatal("want error")
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
}

func TestRunIgnoresNonExecutableAndOtherEvents(t *testing.T) {
	dir := t.TempDir()
	// 実行ビットなし
	if err := os.WriteFile(filepath.Join(dir, "on-create.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScript(t, dir, "on-delete.sh", "#!/bin/sh\nexit 0\n")
	r := NewRunner(dir, t.TempDir(), time.Minute)

	res, err := r.Run(context.Background(), OnCreate, Env{Branch: "pr-1"})
	if err != nil || res.Ran {
		t.Errorf("non-executable hook should be skipped: %+v %v", res, err)
	}
}

func TestRunTimeout(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "on-create.sh", "#!/bin/sh\nsleep 5\n")
	r := NewRunner(dir, t.TempDir(), 200*time.Millisecond)

	_, err := r.Run(context.Background(), OnCreate, Env{Branch: "pr-1"})
	if err == nil {
		t.Fatal("want timeout error")
	}
}
