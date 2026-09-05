// Package storage はブランチの実体(ボリューム)を作る・消す・巻き戻すための
// バックエンド抽象。zfs(ローカル)と fsx(AWS API)で操作レイテンシが2桁違うため、
// コアは速度を仮定せず Capabilities を見て挙動を切り替える。
package storage

import (
	"context"
	"time"
)

// Capabilities はバックエンドの性格の宣言。コアの挙動切り替えに使う。
type Capabilities struct {
	FastRollback  bool          // zfs: true / fsx: false
	TypicalCreate time.Duration // zfs: ~2s / fsx: ~70s
	AsyncDelete   bool          // zfs: false / fsx: true
}

// SnapshotRef はスナップショットの完全修飾名(例: dbpool/base@baseline-20260901)。
type SnapshotRef string

// Volume はブランチ1つ分のデータセット/ボリューム。
type Volume struct {
	Name    string // ブランチ名
	Dataset string // 例: dbpool/branches/pr-123 / fsvol-xxxx
	Path    string // マウント済みパス(この下に data/ がある)
}

// JobID は非同期ジョブの識別子。
type JobID string

// JobStatus は非同期ジョブの状態。
type JobStatus string

const (
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
)

// Storage はバックエンドが実装するインターフェース。
type Storage interface {
	Capabilities() Capabilities

	// Clone はベースラインの snapshot から新しいブランチ用ボリュームを作り、
	// マウント済みの状態で返す。完了まで待つ(fsx なら AdministrativeActions を監視)。
	Clone(ctx context.Context, baseline SnapshotRef, name string) (Volume, error)

	// SnapshotInit はブランチ作成直後(mysqld 起動前)の状態を保存する(reset 用)。
	// FastRollback == false のバックエンドは NopSnapshot を返してよい。
	SnapshotInit(ctx context.Context, vol Volume) (SnapshotRef, error)

	// Rollback は FastRollback == true のときだけ呼ばれる。
	Rollback(ctx context.Context, vol Volume, snap SnapshotRef) error

	// DeleteAsync は削除ジョブを投入して即返る。完了は Poll で確認する。
	DeleteAsync(ctx context.Context, vol Volume) (JobID, error)
	Poll(ctx context.Context, job JobID) (JobStatus, error)

	// SnapshotBase はベースライン更新用。現在の base から新スナップショットを撮る。
	SnapshotBase(ctx context.Context, tag string) (SnapshotRef, error)
	ListSnapshots(ctx context.Context) ([]SnapshotRef, error)

	// UsedBytes はボリュームの現在のディスク消費(CoW差分)。
	UsedBytes(ctx context.Context, vol Volume) (int64, error)
}

// NopSnapshot は SnapshotInit を持たないバックエンドが返す番兵値。
const NopSnapshot SnapshotRef = ""
