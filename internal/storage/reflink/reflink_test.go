//go:build linux

package reflink

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rikukaInoue/sashiki/internal/storage"
)

var (
	_ storage.Storage = (*Backend)(nil)
	_ storage.Renamer = (*Backend)(nil)
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newBackend は base/data に 1 ファイル置いた reflink バックエンドを用意する。
// 実行 FS が reflink 非対応(ext4/overlay 等)なら Skip する。reflink 対応 FS で
// 走らせるには TMPDIR を XFS(reflink=1)/Btrfs マウントに向ける。
func newBackend(t *testing.T) *Backend {
	t.Helper()
	root := t.TempDir()
	b := New(Config{Root: root})
	writeFile(t, filepath.Join(root, "base", "data", "seed.txt"), "hello")
	if _, err := b.SnapshotBase(context.Background(), "probe"); err != nil {
		t.Skipf("reflink not supported on test FS (set TMPDIR to an XFS reflink/Btrfs mount): %v", err)
	}
	_ = os.RemoveAll(filepath.Join(root, "base", "snap", "probe"))
	return b
}

func TestCloneIsIndependentCoW(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	base, err := b.SnapshotBase(ctx, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.CurrentBaseline(); string(got) != string(base) {
		t.Errorf("CurrentBaseline = %q, want %q", got, base)
	}
	vol, err := b.Clone(ctx, base, "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(vol.Path, "data", "seed.txt")); got != "hello" {
		t.Errorf("clone content = %q, want hello", got)
	}
	writeFile(t, filepath.Join(vol.Path, "data", "seed.txt"), "changed")
	if got := readFile(t, filepath.Join(string(base), "seed.txt")); got != "hello" {
		t.Errorf("baseline mutated to %q; clone must be independent", got)
	}
}

func TestSnapshotInitAndRollback(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	base, _ := b.SnapshotBase(ctx, "baseline")
	vol, err := b.Clone(ctx, base, "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := b.SnapshotInit(ctx, vol)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(vol.Path, "data", "seed.txt"), "dirty")
	writeFile(t, filepath.Join(vol.Path, "data", "extra.txt"), "junk")
	if err := b.Rollback(ctx, vol, snap); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(vol.Path, "data", "seed.txt")); got != "hello" {
		t.Errorf("after rollback = %q, want hello", got)
	}
	if _, err := os.Stat(filepath.Join(vol.Path, "data", "extra.txt")); !os.IsNotExist(err) {
		t.Error("rollback should drop files created after the snapshot")
	}
}

func TestRenameAndDelete(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	base, _ := b.SnapshotBase(ctx, "baseline")
	vol, _ := b.Clone(ctx, base, "pr-1")
	renamed, err := b.Rename(ctx, vol, "pr-1-old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vol.Path); !os.IsNotExist(err) {
		t.Error("old path should be gone after rename")
	}
	if got := readFile(t, filepath.Join(renamed.Path, "data", "seed.txt")); got != "hello" {
		t.Errorf("renamed content = %q", got)
	}
	job, err := b.DeleteAsync(ctx, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := b.Poll(ctx, job); st != storage.JobCompleted {
		t.Errorf("Poll = %q, want completed", st)
	}
	if _, err := os.Stat(renamed.Path); !os.IsNotExist(err) {
		t.Error("delete should remove the branch directory")
	}
}
