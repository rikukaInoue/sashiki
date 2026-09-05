// Package fsx は FSx for OpenZFS バックエンド。snapshot/clone を AWS API で
// 行い、ボリュームを NFS でマウントして使う。実測(RESULTS-FSX.md):
// clone 52〜71秒 / restore 10分超 / delete 6分 — コントロールプレーンは
// 「分」の世界なので、Capabilities でそれを宣言しコアに挙動を切り替えさせる。
//
// 完了判定は Volume の Lifecycle ではなく AdministrativeActions を見る
// (restore/clone 中も Lifecycle は AVAILABLE のまま — 実測で datadir を
// 壊した教訓)。
package fsx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	awsfsx "github.com/aws/aws-sdk-go-v2/service/fsx"
	"github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/rikukaInoue/twig/internal/storage"
)

// Config は fsx バックエンドの設定。
type Config struct {
	FileSystemID     string // fs-xxxx
	BaseVolumeID     string // fsvol-xxxx (base)
	ParentVolumeID   string // fsvol-xxxx (root。クローンのぶら下げ先)
	BaselineSnapshot string // baseline (base ボリューム上の snapshot 名)
	DNSName          string // fs-xxxx.fsx.<region>.amazonaws.com
	MountRoot        string // /mnt/twig
	PollInterval     time.Duration
}

// API は fsx バックエンドが使う AWS API のサブセット(テストでモックする)。
type API interface {
	CreateVolume(ctx context.Context, in *awsfsx.CreateVolumeInput, opts ...func(*awsfsx.Options)) (*awsfsx.CreateVolumeOutput, error)
	DeleteVolume(ctx context.Context, in *awsfsx.DeleteVolumeInput, opts ...func(*awsfsx.Options)) (*awsfsx.DeleteVolumeOutput, error)
	DescribeVolumes(ctx context.Context, in *awsfsx.DescribeVolumesInput, opts ...func(*awsfsx.Options)) (*awsfsx.DescribeVolumesOutput, error)
	CreateSnapshot(ctx context.Context, in *awsfsx.CreateSnapshotInput, opts ...func(*awsfsx.Options)) (*awsfsx.CreateSnapshotOutput, error)
	DescribeSnapshots(ctx context.Context, in *awsfsx.DescribeSnapshotsInput, opts ...func(*awsfsx.Options)) (*awsfsx.DescribeSnapshotsOutput, error)
}

// Backend は storage.Storage の FSx 実装。
type Backend struct {
	cfg Config
	api API
	// mount/umount 実行(テストで差し替え)
	mount  func(ctx context.Context, source, target string) error
	umount func(ctx context.Context, target string) error
}

// New は fsx バックエンドを作る。
func New(cfg Config, api API) *Backend {
	if cfg.MountRoot == "" {
		cfg.MountRoot = "/mnt/twig"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	b := &Backend{cfg: cfg, api: api}
	b.mount = b.execMount
	b.umount = b.execUmount
	return b
}

// Capabilities: 実測値の宣言。lazy create は無効化され、reset は
// 「新クローン + 付け替え」の共通経路になる(仕様 15-3)。
func (b *Backend) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		FastRollback:  false,
		TypicalCreate: 70 * time.Second,
		AsyncDelete:   true,
	}
}

func (b *Backend) execMount(ctx context.Context, source, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "mount", "-t", "nfs",
		"-o", "nfsvers=4.1,rsize=1048576,wsize=1048576,hard,noatime",
		source, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount %s: %w: %s", source, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (b *Backend) execUmount(ctx context.Context, target string) error {
	out, err := exec.CommandContext(ctx, "umount", "-l", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CurrentBaseline は base ボリューム上の baseline snapshot の ARN。
func (b *Backend) CurrentBaseline() storage.SnapshotRef {
	arn, err := b.findSnapshotARN(context.Background(), b.cfg.BaselineSnapshot)
	if err != nil {
		return storage.NopSnapshot
	}
	return storage.SnapshotRef(arn)
}

func (b *Backend) findSnapshotARN(ctx context.Context, name string) (string, error) {
	out, err := b.api.DescribeSnapshots(ctx, &awsfsx.DescribeSnapshotsInput{
		Filters: []types.SnapshotFilter{{
			Name:   types.SnapshotFilterNameVolumeId,
			Values: []string{b.cfg.BaseVolumeID},
		}},
	})
	if err != nil {
		return "", err
	}
	for _, s := range out.Snapshots {
		if s.Name != nil && *s.Name == name && s.ResourceARN != nil {
			return *s.ResourceARN, nil
		}
	}
	return "", fmt.Errorf("snapshot %q not found on %s", name, b.cfg.BaseVolumeID)
}

// waitVolumeReady は Lifecycle AVAILABLE かつ AdministrativeActions 全完了を待つ。
func (b *Backend) waitVolumeReady(ctx context.Context, volID string) error {
	for {
		out, err := b.api.DescribeVolumes(ctx, &awsfsx.DescribeVolumesInput{VolumeIds: []string{volID}})
		if err != nil {
			return err
		}
		if len(out.Volumes) == 0 {
			return fmt.Errorf("volume %s not found", volID)
		}
		v := out.Volumes[0]
		ready := v.Lifecycle == types.VolumeLifecycleAvailable
		for _, a := range v.AdministrativeActions {
			if a.Status == types.StatusInProgress || a.Status == types.StatusPending {
				ready = false
			}
			if a.Status == types.StatusFailed {
				return fmt.Errorf("volume %s: administrative action %s failed", volID, a.AdministrativeActionType)
			}
		}
		if v.Lifecycle == types.VolumeLifecycleFailed || v.Lifecycle == types.VolumeLifecycleMisconfigured {
			return fmt.Errorf("volume %s lifecycle: %s", volID, v.Lifecycle)
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.cfg.PollInterval):
		}
	}
}

// Clone はクローンボリュームを作成し、NFS マウントして返す。
// FSx のボリューム名はパス衝突を避けるため世代サフィックス付き
// (name-g<hex>)にし、twig-branch タグで元の名前に紐づける。
// reset の「作り直し + 付け替え」で新旧が一時的に共存できる。
func (b *Backend) Clone(ctx context.Context, baseline storage.SnapshotRef, name string) (storage.Volume, error) {
	gen := make([]byte, 3)
	_, _ = rand.Read(gen)
	fsxName := fmt.Sprintf("%s-g%s", name, hex.EncodeToString(gen))
	out, err := b.api.CreateVolume(ctx, &awsfsx.CreateVolumeInput{
		VolumeType: types.VolumeTypeOpenzfs,
		Name:       &fsxName,
		OpenZFSConfiguration: &types.CreateOpenZFSVolumeConfiguration{
			ParentVolumeId: &b.cfg.ParentVolumeID,
			OriginSnapshot: &types.CreateOpenZFSOriginSnapshotConfiguration{
				SnapshotARN:  (*string)(&baseline),
				CopyStrategy: types.OpenZFSCopyStrategyClone,
			},
			NfsExports: []types.OpenZFSNfsExport{{
				ClientConfigurations: []types.OpenZFSClientConfiguration{{
					Clients: strPtr("*"),
					Options: []string{"rw", "crossmnt", "no_root_squash"},
				}},
			}},
		},
		Tags: []types.Tag{
			{Key: strPtr("twig"), Value: strPtr("true")},
			{Key: strPtr("twig-branch"), Value: &name},
		},
	})
	if err != nil {
		return storage.Volume{}, fmt.Errorf("fsx create-volume: %w", err)
	}
	volID := *out.Volume.VolumeId
	if err := b.waitVolumeReady(ctx, volID); err != nil {
		return storage.Volume{}, err
	}
	volPath := "/fsx/" + fsxName
	if out.Volume.OpenZFSConfiguration != nil && out.Volume.OpenZFSConfiguration.VolumePath != nil {
		volPath = *out.Volume.OpenZFSConfiguration.VolumePath
	}
	target := filepath.Join(b.cfg.MountRoot, fsxName)
	if err := b.mount(ctx, b.cfg.DNSName+":"+volPath, target); err != nil {
		return storage.Volume{}, err
	}
	return storage.Volume{Name: name, Dataset: volID, Path: target}, nil
}

// ResolveVolume は twig-branch タグでボリュームを引く。reset の付け替え中は
// 複数世代が共存し得るため、作成が最新の AVAILABLE を採用する。
func (b *Backend) ResolveVolume(ctx context.Context, name string) (storage.Volume, error) {
	out, err := b.api.DescribeVolumes(ctx, &awsfsx.DescribeVolumesInput{
		Filters: []types.VolumeFilter{{
			Name:   types.VolumeFilterNameFileSystemId,
			Values: []string{b.cfg.FileSystemID},
		}},
	})
	if err != nil {
		return storage.Volume{}, err
	}
	var candidates []types.Volume
	for _, v := range out.Volumes {
		if v.Lifecycle != types.VolumeLifecycleAvailable {
			continue
		}
		for _, t := range v.Tags {
			if t.Key != nil && *t.Key == "twig-branch" && t.Value != nil && *t.Value == name {
				candidates = append(candidates, v)
			}
		}
	}
	if len(candidates) == 0 {
		return storage.Volume{}, fmt.Errorf("volume for branch %q not found", name)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreationTime.After(*candidates[j].CreationTime)
	})
	v := candidates[0]
	return storage.Volume{
		Name:    name,
		Dataset: *v.VolumeId,
		Path:    filepath.Join(b.cfg.MountRoot, *v.Name),
	}, nil
}

// SnapshotInit: fsx は FastRollback を持たないため @init は取得しない
// (reset は「新クローン + 付け替え」)。
func (b *Backend) SnapshotInit(ctx context.Context, vol storage.Volume) (storage.SnapshotRef, error) {
	return storage.NopSnapshot, nil
}

// Rollback は FastRollback=false のため呼ばれない。
func (b *Backend) Rollback(ctx context.Context, vol storage.Volume, snap storage.SnapshotRef) error {
	return errors.New("fsx backend does not support rollback (use recreate)")
}

// DeleteAsync はアンマウントして削除ジョブを投入する(完了まで約6分、Poll で確認)。
func (b *Backend) DeleteAsync(ctx context.Context, vol storage.Volume) (storage.JobID, error) {
	_ = b.umount(ctx, vol.Path)
	_, err := b.api.DeleteVolume(ctx, &awsfsx.DeleteVolumeInput{
		VolumeId: &vol.Dataset,
		OpenZFSConfiguration: &types.DeleteVolumeOpenZFSConfiguration{
			Options: []types.DeleteOpenZFSVolumeOption{
				types.DeleteOpenZFSVolumeOptionDeleteChildVolumesAndSnapshots,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("fsx delete-volume: %w", err)
	}
	return storage.JobID(vol.Dataset), nil
}

// Poll は削除ジョブの完了確認(ボリュームが消えたら completed)。
func (b *Backend) Poll(ctx context.Context, job storage.JobID) (storage.JobStatus, error) {
	out, err := b.api.DescribeVolumes(ctx, &awsfsx.DescribeVolumesInput{VolumeIds: []string{string(job)}})
	if err != nil {
		var nf *types.VolumeNotFound
		if errors.As(err, &nf) {
			return storage.JobCompleted, nil
		}
		return storage.JobFailed, err
	}
	if len(out.Volumes) == 0 {
		return storage.JobCompleted, nil
	}
	if out.Volumes[0].Lifecycle == types.VolumeLifecycleFailed {
		return storage.JobFailed, nil
	}
	return storage.JobRunning, nil
}

// SnapshotBase は base ボリュームの snapshot を取得して ARN を返す。
func (b *Backend) SnapshotBase(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	out, err := b.api.CreateSnapshot(ctx, &awsfsx.CreateSnapshotInput{
		Name:     &tag,
		VolumeId: &b.cfg.BaseVolumeID,
	})
	if err != nil {
		return storage.NopSnapshot, err
	}
	// snapshot の AVAILABLE を待つ
	snapID := *out.Snapshot.SnapshotId
	for {
		ds, err := b.api.DescribeSnapshots(ctx, &awsfsx.DescribeSnapshotsInput{SnapshotIds: []string{snapID}})
		if err != nil {
			return storage.NopSnapshot, err
		}
		if len(ds.Snapshots) > 0 && ds.Snapshots[0].Lifecycle == types.SnapshotLifecycleAvailable {
			return storage.SnapshotRef(*ds.Snapshots[0].ResourceARN), nil
		}
		select {
		case <-ctx.Done():
			return storage.NopSnapshot, ctx.Err()
		case <-time.After(b.cfg.PollInterval):
		}
	}
}

// ListSnapshots は base ボリュームの snapshot 一覧(ARN)。
func (b *Backend) ListSnapshots(ctx context.Context) ([]storage.SnapshotRef, error) {
	out, err := b.api.DescribeSnapshots(ctx, &awsfsx.DescribeSnapshotsInput{
		Filters: []types.SnapshotFilter{{
			Name:   types.SnapshotFilterNameVolumeId,
			Values: []string{b.cfg.BaseVolumeID},
		}},
	})
	if err != nil {
		return nil, err
	}
	var refs []storage.SnapshotRef
	for _, s := range out.Snapshots {
		if s.ResourceARN != nil {
			refs = append(refs, storage.SnapshotRef(*s.ResourceARN))
		}
	}
	return refs, nil
}

// UsedBytes: FSx の API はボリューム単位の使用量を返さない(CloudWatch のみ)。
// v1.0 では 0 を返す(既知の制限)。
func (b *Backend) UsedBytes(ctx context.Context, vol storage.Volume) (int64, error) {
	return 0, nil
}

func strPtr(s string) *string { return &s }
