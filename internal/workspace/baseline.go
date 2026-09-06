// baseline の set / GC(仕様 12-1, 12-2, 12-5)。baseline は main の immutable
// publication。current は pointer にすぎず、publish しても既存 branch の
// origin は変わらない。GC は lineage(どの branch が参照中か)を sashiki が把握し、
// current・参照中は残す。zfs promote は使わない(lineage が追えなくなる)。
package workspace

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/rikukaInoue/sashiki/internal/state"
	"github.com/rikukaInoue/sashiki/internal/storage"
)

// SetBaseline は current pointer を指定 snapshot へ切り替える(仕様 12-2)。
// 直前の正常版へ即 rollback するのに使う。snapshot は登録済みである必要がある。
func (m *Manager) SetBaseline(ctx context.Context, snapshot string) error {
	m.baselineMu.Lock()
	defer m.baselineMu.Unlock()
	b, err := m.db.GetBaseline(snapshot)
	if err != nil {
		return fmt.Errorf("baseline %s is not registered: %w", snapshot, ErrBaselineNotFound)
	}
	// publish ポリシー(refresh と同じ)を set にも課す(抜け道を塞ぐ)。
	if m.baselinePolicy.RequireMasked && !b.Prov.Masked {
		return fmt.Errorf("cannot set baseline %s: not masked (require_masked): %w", snapshot, ErrPreconditionFailed)
	}
	if m.baselinePolicy.RequireValidated && !b.Prov.Validated {
		return fmt.Errorf("cannot set baseline %s: not validated (require_validated): %w", snapshot, ErrPreconditionFailed)
	}
	return m.db.SetCurrentBaseline(snapshot)
}

// GCConfig は baseline GC のポリシー。
type GCConfig struct {
	KeepLast int // 直近 N 個は残す(0 = 無制限に古いのを消す)
}

// GCResult は GC の結果。
type GCResult struct {
	Deleted []string
	Kept    []string
}

// GCBaselines は GC ポリシーに従って古い baseline snapshot を削除する。
// current・branch から参照中・直近 KeepLast は残す。
func (m *Manager) GCBaselines(ctx context.Context, cfg GCConfig) (GCResult, error) {
	// SetBaseline と直列化して「current にしようとしている baseline を GC が消す」
	// 競合を防ぐ(current の読み取りと削除を同一クリティカルセクションで行う)。
	m.baselineMu.Lock()
	defer m.baselineMu.Unlock()
	baselines, err := m.db.ListBaselines()
	if err != nil {
		return GCResult{}, err
	}
	refs, err := m.db.BaselineRefCounts()
	if err != nil {
		return GCResult{}, err
	}
	current := string(m.currentBaseline())

	// 新しい順(ListBaselines は DESC)。KeepLast 個は無条件で残す。
	sort.SliceStable(baselines, func(i, j int) bool {
		return baselines[i].CreatedAt.After(baselines[j].CreatedAt)
	})

	var res GCResult
	for i, b := range baselines {
		keep := false
		switch {
		case b.Snapshot == current:
			keep = true // current pointer
		case b.IsCurrent:
			keep = true
		case refs[b.Snapshot] > 0:
			keep = true // branch が参照中
		case cfg.KeepLast > 0 && i < cfg.KeepLast:
			keep = true // 直近 N
		}
		if keep {
			res.Kept = append(res.Kept, b.Snapshot)
			continue
		}
		// storage から snapshot を削除(DeleteBaselineSnapshot を持つバックエンドのみ)。
		if bd, ok := m.st.(baselineDeleter); ok {
			if err := bd.DeleteBaselineSnapshot(ctx, storage.SnapshotRef(b.Snapshot)); err != nil {
				log.Printf("baseline gc: delete %s: %v", b.Snapshot, err)
				res.Kept = append(res.Kept, b.Snapshot)
				continue
			}
		}
		if err := m.db.DeleteBaseline(b.Snapshot); err != nil {
			return res, err
		}
		res.Deleted = append(res.Deleted, b.Snapshot)
	}
	return res, nil
}

// baselineDeleter は baseline snapshot を削除できるバックエンド。
type baselineDeleter interface {
	DeleteBaselineSnapshot(ctx context.Context, snap storage.SnapshotRef) error
}

// ListBaselineRows は API 用に provenance 付きで baseline を返す。
func (m *Manager) ListBaselineRows() ([]state.BaselineRow, error) {
	return m.db.ListBaselines()
}
