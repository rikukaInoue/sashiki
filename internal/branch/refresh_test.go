package branch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeRefreshScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "refresh.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitRefreshDone(t *testing.T) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if !RefreshInProgress() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("refresh did not finish")
}

func quiesceOK(ctx context.Context) error   { return nil }
func quiesceBusy(ctx context.Context) error { return errors.New("mysqld still running") }

func TestRefreshSuccessRotatesBaseline(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	script := writeRefreshScript(t, "exit 0")
	tag, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() != "" {
		t.Fatalf("refresh error: %s", RefreshLastError())
	}
	// current が新 snapshot(pool/base@tag)に切り替わっている
	info, err := m.Baseline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "pool/base@" + tag
	if info.Current != want {
		t.Errorf("current = %q, want %q", info.Current, want)
	}
	// 以降の Create は新 baseline を origin にする
	created, err := m.Create(context.Background(), "pr-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if created.OriginSnapshot != want {
		t.Errorf("origin = %q, want %q", created.OriginSnapshot, want)
	}
}

func TestRefreshScriptFailureDoesNotSnapshot(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	before, _ := m.Baseline(context.Background())
	script := writeRefreshScript(t, "echo boom >&2; exit 3")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK,
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() == "" {
		t.Error("last error should be recorded")
	}
	after, _ := m.Baseline(context.Background())
	if after.Current != before.Current {
		t.Errorf("baseline should not rotate on failure: %q -> %q", before.Current, after.Current)
	}
}

func TestRefreshAbortsWhenNotQuiesced(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	before, _ := m.Baseline(context.Background())
	// スクリプトは exit 0 だが quiesce 検証が失敗し続ける → snapshot 取得しない
	script := writeRefreshScript(t, "exit 0")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceBusy,
	}); err != nil {
		t.Fatal(err)
	}
	waitRefreshDone(t)
	if RefreshLastError() == "" {
		t.Error("quiesce failure should be recorded")
	}
	after, _ := m.Baseline(context.Background())
	if after.Current != before.Current {
		t.Error("baseline must not rotate when base is not quiesced")
	}
}

func TestRefreshRejectsConcurrentRun(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	script := writeRefreshScript(t, "sleep 0.5")
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RefreshBaseline(context.Background(), RefreshConfig{
		Script: script, CheckQuiesced: quiesceOK,
	}); !errors.Is(err, ErrRefreshRunning) {
		t.Errorf("err = %v, want ErrRefreshRunning", err)
	}
	waitRefreshDone(t)
}
