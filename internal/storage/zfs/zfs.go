// Package zfs はローカル OpenZFS バックエンド。zfs/zpool コマンドを実行する。
// twigd の実行ユーザーには sudoers で /usr/sbin/zfs のみを許可する想定
// (deploy/systemd/README 参照)。
package zfs

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rikukaInoue/twig/internal/storage"
)

// Config は zfs バックエンドの設定。
type Config struct {
	Pool             string // dbpool
	BaseDataset      string // dbpool/base
	BranchParent     string // dbpool/branches
	BaselineSnapshot string // baseline (dbpool/base@baseline)
	ZfsBin           string // 既定 "zfs"。テストで差し替える
	Sudo             bool   // true なら sudo 経由で実行
}

// Backend は storage.Storage の zfs 実装。
type Backend struct {
	cfg Config
	run runner
}

type runner func(ctx context.Context, args ...string) (string, error)

// New は zfs バックエンドを作る。
func New(cfg Config) *Backend {
	if cfg.ZfsBin == "" {
		cfg.ZfsBin = "zfs"
	}
	b := &Backend{cfg: cfg}
	b.run = b.execZfs
	return b
}

func (b *Backend) execZfs(ctx context.Context, args ...string) (string, error) {
	var cmd *exec.Cmd
	if b.cfg.Sudo {
		cmd = exec.CommandContext(ctx, "sudo", append([]string{"-n", b.cfg.ZfsBin}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, b.cfg.ZfsBin, args...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("zfs %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Capabilities はローカル zfs の性格: 全操作が速い。
func (b *Backend) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		FastRollback:  true,
		TypicalCreate: 2 * time.Second,
		AsyncDelete:   false,
	}
}

func (b *Backend) branchDataset(name string) string {
	return b.cfg.BranchParent + "/" + name
}

// CurrentBaseline は現在のベースライン snapshot の完全修飾名。
func (b *Backend) CurrentBaseline() storage.SnapshotRef {
	return storage.SnapshotRef(b.cfg.BaseDataset + "@" + b.cfg.BaselineSnapshot)
}

// Clone は zfs clone してマウントポイントを返す。
func (b *Backend) Clone(ctx context.Context, baseline storage.SnapshotRef, name string) (storage.Volume, error) {
	ds := b.branchDataset(name)
	if _, err := b.run(ctx, "clone", string(baseline), ds); err != nil {
		return storage.Volume{}, err
	}
	mp, err := b.run(ctx, "get", "-H", "-o", "value", "mountpoint", ds)
	if err != nil {
		return storage.Volume{}, err
	}
	return storage.Volume{Name: name, Dataset: ds, Path: mp}, nil
}

// ResolveVolume は既存ブランチのデータセットとマウントポイントを引く。
func (b *Backend) ResolveVolume(ctx context.Context, name string) (storage.Volume, error) {
	ds := b.branchDataset(name)
	mp, err := b.run(ctx, "get", "-H", "-o", "value", "mountpoint", ds)
	if err != nil {
		return storage.Volume{}, err
	}
	return storage.Volume{Name: name, Dataset: ds, Path: mp}, nil
}

// SnapshotInit は clone 直後(mysqld 起動前)に @init を取得する。
func (b *Backend) SnapshotInit(ctx context.Context, vol storage.Volume) (storage.SnapshotRef, error) {
	snap := vol.Dataset + "@init"
	if _, err := b.run(ctx, "snapshot", snap); err != nil {
		return storage.NopSnapshot, err
	}
	return storage.SnapshotRef(snap), nil
}

// Rollback は @init への巻き戻し。-r で @init より後のスナップショットも破棄する。
func (b *Backend) Rollback(ctx context.Context, vol storage.Volume, snap storage.SnapshotRef) error {
	_, err := b.run(ctx, "rollback", "-r", string(snap))
	return err
}

// DeleteAsync は zfs destroy。ローカル zfs は同期で即完了するため、
// 完了済みジョブを返す。
func (b *Backend) DeleteAsync(ctx context.Context, vol storage.Volume) (storage.JobID, error) {
	if _, err := b.run(ctx, "destroy", "-r", vol.Dataset); err != nil {
		return "", err
	}
	return storage.JobID("done:" + vol.Dataset), nil
}

// Poll は DeleteAsync が同期完了しているため常に completed。
func (b *Backend) Poll(ctx context.Context, job storage.JobID) (storage.JobStatus, error) {
	return storage.JobCompleted, nil
}

// SnapshotBase は base から新しいベースライン snapshot を取得する。
func (b *Backend) SnapshotBase(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	snap := b.cfg.BaseDataset + "@" + tag
	if _, err := b.run(ctx, "snapshot", snap); err != nil {
		return storage.NopSnapshot, err
	}
	return storage.SnapshotRef(snap), nil
}

// ListSnapshots は base のスナップショット一覧。
func (b *Backend) ListSnapshots(ctx context.Context) ([]storage.SnapshotRef, error) {
	out, err := b.run(ctx, "list", "-H", "-t", "snapshot", "-o", "name", "-r", b.cfg.BaseDataset)
	if err != nil {
		return nil, err
	}
	var refs []storage.SnapshotRef
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			refs = append(refs, storage.SnapshotRef(line))
		}
	}
	return refs, nil
}

// UsedBytes は zfs get used の値。
func (b *Backend) UsedBytes(ctx context.Context, vol storage.Volume) (int64, error) {
	out, err := b.run(ctx, "get", "-H", "-p", "-o", "value", "used", vol.Dataset)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(out, 10, 64)
}
