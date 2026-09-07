//go:build linux

package reflink

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rikukaInoue/sashiki/internal/storage"
)

// Backend は storage.Storage の reflink 実装(`cp --reflink=always`)。
type Backend struct {
	cfg Config
}

// New は reflink バックエンドを作る。
func New(cfg Config) *Backend {
	if cfg.CpBin == "" {
		cfg.CpBin = "cp"
	}
	if cfg.BaselineSnapshot == "" {
		cfg.BaselineSnapshot = "baseline"
	}
	return &Backend{cfg: cfg}
}

// cloneTree は src を dst へ reflink(CoW)複製する。dst は存在しないこと。
// `--reflink=always` は CoW できなければ失敗する(黙って通常コピーに退行しない)。
func (b *Backend) cloneTree(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, b.cfg.CpBin, "-a", "--reflink=always", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("reflink %s -> %s: %w: %s", src, dst, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (b *Backend) branchDir(name string) string { return filepath.Join(b.cfg.Root, "branches", name) }
func dataDir(path string) string                { return filepath.Join(path, "data") }

// Capabilities: reflink はローカルで速く、固定名。
func (b *Backend) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		FastRollback:      true,
		TypicalCreate:     500 * time.Millisecond,
		AsyncDelete:       false,
		ClonesAreDistinct: false,
	}
}

// Clone はベースライン snapshot からブランチの datadir を reflink 複製する。
func (b *Backend) Clone(ctx context.Context, baseline storage.SnapshotRef, name string) (storage.Volume, error) {
	dst := b.branchDir(name)
	if _, err := os.Stat(dst); err == nil {
		return storage.Volume{}, fmt.Errorf("branch %q already exists at %s", name, dst)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return storage.Volume{}, err
	}
	if err := b.cloneTree(ctx, string(baseline), dataDir(dst)); err != nil {
		_ = os.RemoveAll(dst)
		return storage.Volume{}, err
	}
	return storage.Volume{Name: name, Dataset: dst, Path: dst}, nil
}

// ResolveVolume は既存ブランチの Volume を名前規約から再構成する
// (reset/recreate 等で manager が使う)。
func (b *Backend) ResolveVolume(ctx context.Context, name string) (storage.Volume, error) {
	dst := b.branchDir(name)
	return storage.Volume{Name: name, Dataset: dst, Path: dst}, nil
}

// SnapshotInit は作成直後の datadir を .init として reflink クローンする(reset 用)。
func (b *Backend) SnapshotInit(ctx context.Context, vol storage.Volume) (storage.SnapshotRef, error) {
	snap := filepath.Join(vol.Path, ".init")
	if err := os.RemoveAll(snap); err != nil {
		return "", err
	}
	if err := b.cloneTree(ctx, dataDir(vol.Path), snap); err != nil {
		return "", err
	}
	return storage.SnapshotRef(snap), nil
}

// Rollback は datadir を snapshot の reflink クローンで置き換える。
func (b *Backend) Rollback(ctx context.Context, vol storage.Volume, snap storage.SnapshotRef) error {
	data := dataDir(vol.Path)
	if err := os.RemoveAll(data); err != nil {
		return err
	}
	return b.cloneTree(ctx, string(snap), data)
}

// DeleteAsync はブランチのディレクトリを削除する(同期削除して完了済みジョブを返す)。
func (b *Backend) DeleteAsync(ctx context.Context, vol storage.Volume) (storage.JobID, error) {
	if err := os.RemoveAll(vol.Path); err != nil {
		return "", err
	}
	return storage.JobID("done"), nil
}

func (b *Backend) Poll(ctx context.Context, job storage.JobID) (storage.JobStatus, error) {
	return storage.JobCompleted, nil
}

// SnapshotBase は base/data から新しいベースライン snapshot を reflink クローンする。
func (b *Backend) SnapshotBase(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	snap := filepath.Join(b.cfg.Root, "base", "snap", tag)
	if err := os.RemoveAll(snap); err != nil {
		return "", err
	}
	if err := b.cloneTree(ctx, filepath.Join(b.cfg.Root, "base", "data"), snap); err != nil {
		return "", err
	}
	return storage.SnapshotRef(snap), nil
}

// PromoteBranch は branch の datadir を新しい baseline snapshot として reflink
// する(#129)。呼び出し側が branch mysqld を停止済みであることを前提とする。
func (b *Backend) PromoteBranch(ctx context.Context, branch storage.Volume, tag string) (storage.SnapshotRef, error) {
	snap := filepath.Join(b.cfg.Root, "base", "snap", tag)
	if err := os.RemoveAll(snap); err != nil {
		return "", err
	}
	if err := b.cloneTree(ctx, dataDir(branch.Path), snap); err != nil {
		return "", err
	}
	return storage.SnapshotRef(snap), nil
}

// ListSnapshots は base/snap 配下のベースライン snapshot を列挙する。
func (b *Backend) ListSnapshots(ctx context.Context) ([]storage.SnapshotRef, error) {
	dir := filepath.Join(b.cfg.Root, "base", "snap")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var refs []storage.SnapshotRef
	for _, e := range ents {
		if e.IsDir() {
			refs = append(refs, storage.SnapshotRef(filepath.Join(dir, e.Name())))
		}
	}
	return refs, nil
}

// UsedBytes は branch の CoW 差分。reflink の共有エクステント差分だけを素直に
// 取得する手段が無く、du は全量を返して誤解を招くため -1(不明)を返す。表示側は
// 「-」を出す(#128)。
func (b *Backend) UsedBytes(ctx context.Context, vol storage.Volume) (int64, error) {
	return -1, nil
}

// Rename は recreate の退避に使う(storage.Renamer)。
func (b *Backend) Rename(ctx context.Context, vol storage.Volume, newName string) (storage.Volume, error) {
	dst := b.branchDir(newName)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return storage.Volume{}, err
	}
	if err := os.Rename(vol.Path, dst); err != nil {
		return storage.Volume{}, err
	}
	return storage.Volume{Name: newName, Dataset: dst, Path: dst}, nil
}

// DeleteBaselineSnapshot はベースライン snapshot を削除する(baseline GC 用)。
func (b *Backend) DeleteBaselineSnapshot(ctx context.Context, snap storage.SnapshotRef) error {
	return os.RemoveAll(string(snap))
}

// CurrentBaseline は現在のベースライン snapshot パスを返す(BaselineProvider)。
func (b *Backend) CurrentBaseline() storage.SnapshotRef {
	return storage.SnapshotRef(filepath.Join(b.cfg.Root, "base", "snap", b.cfg.BaselineSnapshot))
}
