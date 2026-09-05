package branch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rikukaInoue/twig/internal/engine"
	"github.com/rikukaInoue/twig/internal/hooks"
	"github.com/rikukaInoue/twig/internal/state"
	"github.com/rikukaInoue/twig/internal/storage"
)

// --- mocks ---

type mockStorage struct {
	caps        storage.Capabilities
	cloned      []string
	snapshots   []string
	rollbacks   []string
	destroyed   []string
	cloneErr    error
	rollbackErr error
}

func (m *mockStorage) Capabilities() storage.Capabilities { return m.caps }

func (m *mockStorage) Clone(ctx context.Context, baseline storage.SnapshotRef, name string) (storage.Volume, error) {
	if m.cloneErr != nil {
		return storage.Volume{}, m.cloneErr
	}
	m.cloned = append(m.cloned, name)
	return storage.Volume{Name: name, Dataset: "pool/branches/" + name, Path: "/pool/branches/" + name}, nil
}

func (m *mockStorage) ResolveVolume(ctx context.Context, name string) (storage.Volume, error) {
	return storage.Volume{Name: name, Dataset: "pool/branches/" + name, Path: "/pool/branches/" + name}, nil
}

func (m *mockStorage) SnapshotInit(ctx context.Context, vol storage.Volume) (storage.SnapshotRef, error) {
	snap := vol.Dataset + "@init"
	m.snapshots = append(m.snapshots, snap)
	return storage.SnapshotRef(snap), nil
}

func (m *mockStorage) Rollback(ctx context.Context, vol storage.Volume, snap storage.SnapshotRef) error {
	if m.rollbackErr != nil {
		return m.rollbackErr
	}
	m.rollbacks = append(m.rollbacks, string(snap))
	return nil
}

func (m *mockStorage) DeleteAsync(ctx context.Context, vol storage.Volume) (storage.JobID, error) {
	m.destroyed = append(m.destroyed, vol.Dataset)
	return "done", nil
}

func (m *mockStorage) Poll(ctx context.Context, job storage.JobID) (storage.JobStatus, error) {
	return storage.JobCompleted, nil
}

func (m *mockStorage) SnapshotBase(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	return storage.SnapshotRef("pool/base@" + tag), nil
}

func (m *mockStorage) ListSnapshots(ctx context.Context) ([]storage.SnapshotRef, error) {
	return nil, nil
}

func (m *mockStorage) UsedBytes(ctx context.Context, vol storage.Volume) (int64, error) {
	return 1024, nil
}

func (m *mockStorage) CurrentBaseline() storage.SnapshotRef { return "pool/base@baseline" }

type mockEngine struct {
	started  []string
	stopped  []string
	startErr error
}

func (m *mockEngine) Start(ctx context.Context, ins engine.Instance) error {
	if m.startErr != nil {
		return m.startErr
	}
	m.started = append(m.started, ins.Branch)
	return nil
}
func (m *mockEngine) Stop(ctx context.Context, ins engine.Instance) error {
	m.stopped = append(m.stopped, ins.Branch)
	return nil
}
func (m *mockEngine) WaitReady(ctx context.Context, ins engine.Instance) error { return nil }
func (m *mockEngine) IsRunning(ctx context.Context, ins engine.Instance) (bool, error) {
	return false, nil
}

// --- helpers ---

func newTestManager(t *testing.T, st *mockStorage, eng *mockEngine, hooksDir string) *Manager {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var hr *hooks.Runner
	if hooksDir != "" {
		hr = hooks.NewRunner(hooksDir, t.TempDir(), time.Minute)
	}
	m, err := New(Config{
		NamePattern: `^[a-z0-9-]{1,32}$`,
		MaxBranches: 3,
		PortLow:     3401,
		PortHigh:    3403,
		EngineType:  "mysql",
		StateDir:    t.TempDir(),
	}, st, st, eng, hr, db)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newTestManagerCfg(t *testing.T, st *mockStorage, eng *mockEngine, hooksDir string, mod func(*Config)) *Manager {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var hr *hooks.Runner
	if hooksDir != "" {
		hr = hooks.NewRunner(hooksDir, t.TempDir(), time.Minute)
	}
	cfg := Config{
		NamePattern: `^[a-z0-9-]{1,32}$`,
		MaxBranches: 3,
		PortLow:     3401,
		PortHigh:    3403,
		EngineType:  "mysql",
		StateDir:    t.TempDir(),
	}
	if mod != nil {
		mod(&cfg)
	}
	m, err := New(cfg, st, st, eng, hr, db)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// --- tests ---

func TestCreateHappyPath(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, "")

	info, err := m.Create(context.Background(), "pr-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s, want running", info.State)
	}
	if info.Port != 3401 {
		t.Errorf("port = %d, want 3401", info.Port)
	}
	if len(st.cloned) != 1 || st.cloned[0] != "pr-1" {
		t.Errorf("cloned = %v", st.cloned)
	}
	// hook なし経路: @init は起動前に撮られ、start は 1 回だけ
	if len(st.snapshots) != 1 {
		t.Errorf("snapshots = %v", st.snapshots)
	}
	if len(eng.started) != 1 {
		t.Errorf("started = %v", eng.started)
	}
	if len(eng.stopped) != 0 {
		t.Errorf("stopped = %v, want none", eng.stopped)
	}
}

func TestCreateInvalidName(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	for _, name := range []string{"", "UPPER", "has_underscore", "日本語", "a b"} {
		if _, err := m.Create(context.Background(), name, 0); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Create(%q) err = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestCreateDuplicate(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), "pr-1", 0); !errors.Is(err, ErrExists) {
		t.Errorf("err = %v, want ErrExists", err)
	}
}

func TestCreatePortAllocation(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	a, _ := m.Create(context.Background(), "pr-1", 0)
	b, _ := m.Create(context.Background(), "pr-2", 0)
	if a.Port == b.Port {
		t.Errorf("duplicate ports: %d", a.Port)
	}
	// 明示ポート指定
	c, err := m.Create(context.Background(), "pr-3", 3403)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 3403 {
		t.Errorf("port = %d, want 3403", c.Port)
	}
}

func TestCreateLimitAndPortExhaustion(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	for i := 1; i <= 3; i++ {
		if _, err := m.Create(context.Background(), fmt.Sprintf("pr-%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Create(context.Background(), "pr-4", 0); !errors.Is(err, ErrLimitReached) {
		t.Errorf("err = %v, want ErrLimitReached", err)
	}
}

func TestCreateStorageFailureLeavesErrorState(t *testing.T) {
	st := &mockStorage{cloneErr: errors.New("boom")}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err == nil {
		t.Fatal("want error")
	}
	// error 状態で残る(自動削除しない)
	info, err := m.Get(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateError {
		t.Errorf("state = %s, want error", info.State)
	}
}

func TestCreateWithHookTakesCleanInitSnapshot(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "on-create.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &mockStorage{}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, dir)

	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	// hook あり経路: start → hook → stop → @init → start
	if len(eng.started) != 2 {
		t.Errorf("started %d times, want 2", len(eng.started))
	}
	if len(eng.stopped) != 1 {
		t.Errorf("stopped %d times, want 1 (clean stop before @init)", len(eng.stopped))
	}
	if len(st.snapshots) != 1 {
		t.Errorf("snapshots = %v", st.snapshots)
	}
}

func TestCreateHookFailureLeavesErrorState(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "on-create.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, dir)

	if _, err := m.Create(context.Background(), "pr-1", 0); err == nil {
		t.Fatal("want error")
	}
	info, _ := m.Get(context.Background(), "pr-1")
	if info.State != state.StateError {
		t.Errorf("state = %s, want error", info.State)
	}
	if info.HookStatus["on-create"] != "failed(7)" {
		t.Errorf("hook status = %v", info.HookStatus)
	}
}

func TestResetRollsBackToInit(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true}}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	info, err := m.Reset(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s", info.State)
	}
	if len(st.rollbacks) != 1 || st.rollbacks[0] != "pool/branches/pr-1@init" {
		t.Errorf("rollbacks = %v", st.rollbacks)
	}
}

func TestResetUnsupportedOnSlowBackend(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: false}}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reset(context.Background(), "pr-1"); err == nil {
		t.Fatal("want error for non-FastRollback backend")
	}
}

func TestDeleteRemovesBranchAndFreesPort(t *testing.T) {
	st := &mockStorage{}
	m := newTestManager(t, st, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), "pr-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(context.Background(), "pr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if len(st.destroyed) != 1 {
		t.Errorf("destroyed = %v", st.destroyed)
	}
	// ポートが解放されて再利用できる
	info, err := m.Create(context.Background(), "pr-2", 3401)
	if err != nil {
		t.Fatal(err)
	}
	if info.Port != 3401 {
		t.Errorf("port = %d", info.Port)
	}
}

func TestDeleteNotFound(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if err := m.Delete(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRouteBranchLazyCreate(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{FastRollback: true, TypicalCreate: 2 * time.Second}}
	eng := &mockEngine{}
	m := newTestManagerCfg(t, st, eng, "", func(c *Config) { c.LazyCreate = true })

	port, err := m.RouteBranch(context.Background(), "pr-9")
	if err != nil {
		t.Fatal(err)
	}
	if port != 3401 {
		t.Errorf("port = %d", port)
	}
	info, _ := m.Get(context.Background(), "pr-9")
	if info.State != state.StateRunning {
		t.Errorf("state = %s", info.State)
	}
	// 2 回目は既存を返す(再作成しない)
	if _, err := m.RouteBranch(context.Background(), "pr-9"); err != nil {
		t.Fatal(err)
	}
	if len(st.cloned) != 1 {
		t.Errorf("cloned %d times, want 1", len(st.cloned))
	}
}

func TestRouteBranchLazyDisabled(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) { c.LazyCreate = false })
	if _, err := m.RouteBranch(context.Background(), "pr-9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRouteBranchLazyGatedBySlowBackend(t *testing.T) {
	st := &mockStorage{caps: storage.Capabilities{TypicalCreate: 70 * time.Second}}
	m := newTestManagerCfg(t, st, &mockEngine{}, "", func(c *Config) {
		c.LazyCreate = true
		c.LazyMaxWait = 20 * time.Second
	})
	// fsx 級の遅いバックエンドでは lazy create は無効(仕様 15-3)
	if _, err := m.RouteBranch(context.Background(), "pr-9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestWakeStartsStoppedBranch(t *testing.T) {
	st := &mockStorage{}
	eng := &mockEngine{}
	m := newTestManager(t, st, eng, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateSleeping, "")
	started := len(eng.started)
	info, err := m.Wake(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != state.StateRunning {
		t.Errorf("state = %s", info.State)
	}
	if len(eng.started) != started+1 {
		t.Errorf("engine should be started once more")
	}
}

func TestRouteBranchWaitsForCreating(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.LazyCreate = true
		c.LazyMaxWait = 2 * time.Second
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	// creating 状態に巻き戻して、別 goroutine が完了させるのを待つ挙動を再現
	_ = m.db.SetState("pr-1", state.StateCreating, "")
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = m.db.SetState("pr-1", state.StateRunning, "")
	}()
	port, err := m.RouteBranch(context.Background(), "pr-1")
	if err != nil {
		t.Fatalf("should wait for creating branch: %v", err)
	}
	if port != 3401 {
		t.Errorf("port = %d", port)
	}
}

func TestWakeRejectsErrorAndDeleting(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{state.StateError, state.StateDeleting, state.StateCreating} {
		_ = m.db.SetState("pr-1", st, "")
		if _, err := m.Wake(context.Background(), "pr-1"); err == nil {
			t.Errorf("Wake should reject state %s", st)
		}
	}
}

func TestReapIdleStopAndTTLDelete(t *testing.T) {
	st := &mockStorage{}
	eng := &mockEngine{}
	m := newTestManagerCfg(t, st, eng, "", func(c *Config) {
		c.IdleStopAfter = 10 * time.Millisecond
		c.DeleteAfterIdle = time.Hour
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, _ := m.Get(context.Background(), "pr-1")
	if info.State != state.StateSleeping {
		t.Errorf("state = %s, want sleeping", info.State)
	}
	if len(eng.stopped) != 1 {
		t.Errorf("engine stopped %d times", len(eng.stopped))
	}

	// TTL: delete_after_idle を縮めて再パス → 削除される
	m.cfg.DeleteAfterIdle = 10 * time.Millisecond
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(context.Background(), "pr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound after TTL delete", err)
	}
}

func TestReapKeepsErrorBranches(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.DeleteAfterIdle = time.Nanosecond
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); err != nil {
		t.Fatal(err)
	}
	_ = m.db.SetState("pr-1", state.StateError, "boom")
	time.Sleep(5 * time.Millisecond)
	if err := m.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(context.Background(), "pr-1"); err != nil {
		t.Errorf("error branch should be kept: %v", err)
	}
}

func TestMemoryGuardRejectsCreate(t *testing.T) {
	m := newTestManagerCfg(t, &mockStorage{}, &mockEngine{}, "", func(c *Config) {
		c.BufferPoolBytes = 256 * 1024 * 1024
		c.AvailableMem = func() (int64, error) { return 100 * 1024 * 1024, nil } // 100MB
	})
	if _, err := m.Create(context.Background(), "pr-1", 0); !errors.Is(err, ErrLimitReached) {
		t.Errorf("err = %v, want ErrLimitReached", err)
	}
	// 十分な空きなら通る
	m.cfg.AvailableMem = func() (int64, error) { return 2 << 30, nil }
	if _, err := m.Create(context.Background(), "pr-2", 0); err != nil {
		t.Fatal(err)
	}
}
