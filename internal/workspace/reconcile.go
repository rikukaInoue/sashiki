// reconciliation / doctor / orphan GC(仕様 20-1, 20-2)。
// sashikid 再起動後、state.db / ZFS dataset / mysqld プロセス / systemd unit は
// 食い違い得る。起動時に必ず突き合わせる。
package workspace

import (
	"context"
	"fmt"
	"log"

	"github.com/rikukaInoue/sashiki/internal/state"
	"github.com/rikukaInoue/sashiki/internal/storage"
)

// ReconcileReport は突合結果。
type ReconcileReport struct {
	Demoted []string // running → sleeping(プロセス死亡・volume 健全)
	Errored []string // dataset 消失など
	Orphans []string // dataset だけあって state.db に無い branch 名
}

// Reconcile は state.db を ZFS / engine と突き合わせて整合させる(起動時に呼ぶ)。
func (m *Manager) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var rep ReconcileReport
	branches, err := m.db.ListBranches()
	if err != nil {
		return rep, err
	}
	known := map[string]bool{}
	for _, b := range branches {
		known[b.Name] = true
		vol, verr := m.resolveVolume(ctx, b)
		if verr != nil {
			// dataset が無い → recoverable=false の error(手動で消された等)
			_ = m.db.SetError(b.Name, "reconcile", "dataset_missing", false,
				"dataset not found for branch", []string{"`sashiki delete " + b.Name + "` で行を掃除"})
			rep.Errored = append(rep.Errored, b.Name)
			continue
		}
		// running なのにプロセスが死んでいる → sleeping(volume は健全)
		if b.State == state.StateRunning {
			if running, _ := m.eng.IsRunning(ctx, m.instance(b, vol)); !running {
				_ = m.db.SetState(b.Name, state.StateSleeping, "")
				rep.Demoted = append(rep.Demoted, b.Name)
			}
		}
	}
	// dataset だけあって state.db に無い → orphan
	if vl, ok := m.st.(storage.VolumeLister); ok {
		vols, verr := vl.ListBranchVolumes(ctx)
		if verr == nil {
			for _, name := range vols {
				// 予約名(_validate 等)は orphan 扱いしない(state.db 非登録でも正当)。
				if IsReserved(name) {
					continue
				}
				if !known[name] {
					rep.Orphans = append(rep.Orphans, name)
				}
			}
		}
	}
	if len(rep.Demoted)+len(rep.Errored)+len(rep.Orphans) > 0 {
		log.Printf("reconcile: demoted=%v errored=%v orphans=%v", rep.Demoted, rep.Errored, rep.Orphans)
	}
	return rep, nil
}

// GCOrphans は state.db に無い dataset(orphan)を削除する(sashiki gc --orphans)。
func (m *Manager) GCOrphans(ctx context.Context) ([]string, error) {
	rep, err := m.Reconcile(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, name := range rep.Orphans {
		vol, verr := m.resolveVolume(ctx, state.Branch{Name: name})
		if verr != nil {
			continue
		}
		if job, derr := m.st.DeleteAsync(ctx, vol); derr == nil {
			_, _ = m.st.Poll(ctx, job)
			deleted = append(deleted, name)
		}
	}
	return deleted, nil
}

// DoctorReport は sashiki doctor の診断結果。
type DoctorReport struct {
	PoolHealthy     bool
	PoolUsedRatio   float64
	CurrentBaseline string
	BranchCount     int
	PortConflicts   []string
	Orphans         []string
	MemHeadroomOK   bool
	Issues          []string
}

// Doctor は運用者向けの健全性レポートを返す(仕様 20-2)。
func (m *Manager) Doctor(ctx context.Context) (DoctorReport, error) {
	var d DoctorReport
	branches, err := m.db.ListBranches()
	if err != nil {
		return d, err
	}
	d.BranchCount = len(branches)
	d.CurrentBaseline = string(m.currentBaseline())
	if d.CurrentBaseline == "" {
		d.Issues = append(d.Issues, "current baseline が未設定(baseline import/refresh が必要)")
	}

	// port 重複
	seen := map[int]string{}
	for _, b := range branches {
		if prev, ok := seen[b.Port]; ok {
			d.PortConflicts = append(d.PortConflicts, fmt.Sprintf("port %d: %s と %s", b.Port, prev, b.Name))
		}
		seen[b.Port] = b.Name
	}
	d.Issues = append(d.Issues, mapToIssues("port conflict", d.PortConflicts)...)

	// pool 使用率
	if cr, ok := m.st.(storage.CapacityReporter); ok {
		if used, total, err := cr.PoolCapacity(ctx); err == nil && total > 0 {
			d.PoolHealthy = true
			d.PoolUsedRatio = float64(used) / float64(total)
			if m.cfg.CriticalWatermark > 0 && d.PoolUsedRatio >= m.cfg.CriticalWatermark {
				d.Issues = append(d.Issues, fmt.Sprintf("pool 使用率が critical: %.0f%%", d.PoolUsedRatio*100))
			}
		}
	}

	// orphan
	if rep, err := m.Reconcile(ctx); err == nil {
		d.Orphans = rep.Orphans
		d.Issues = append(d.Issues, mapToIssues("orphan dataset", rep.Orphans)...)
	}

	// memory headroom
	if m.cfg.AvailableMem != nil {
		if err := m.admitMemory("doctor"); err == nil {
			d.MemHeadroomOK = true
		} else {
			d.Issues = append(d.Issues, "memory headroom 不足: "+err.Error())
		}
	} else {
		d.MemHeadroomOK = true
	}
	return d, nil
}

func mapToIssues(kind string, items []string) []string {
	var out []string
	for _, i := range items {
		out = append(out, kind+": "+i)
	}
	return out
}
