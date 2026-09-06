// リーパー: アイドルブランチの mysqld を停止(sleeping)し、長期未使用の
// ブランチを削除する。メモリを消費するのは「動いている mysqld」であって
// ブランチのデータではない(PoC 実測: 1 本 ≈ 400MB)ため、停止だけで
// メモリ上限は「同時アクティブ数」にのみ比例するようになる。
package workspace

import (
	"context"
	"time"

	"github.com/rikukaInoue/sashiki/internal/obs"
	"github.com/rikukaInoue/sashiki/internal/state"
)

// RunReaper は interval ごとに Reap を回す。ctx キャンセルで止まる。
func (m *Manager) RunReaper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Reap(ctx); err != nil {
				obs.Log(ctx).Error("reaper pass failed", "component", "reaper", "error", err.Error())
			}
		}
	}
}

// Reap は 1 パス分の回収を行う。
//   - running かつ IdleStopAfter 以上接続なし → 正常終了して sleeping
//   - running/sleeping かつ DeleteAfterIdle 以上接続なし → 削除
//     (error 状態はログ確認のため自動削除しない: 仕様 14-2)
func (m *Manager) Reap(ctx context.Context) error {
	branches, err := m.db.ListBranches()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, b := range branches {
		// 長寿命接続を張ったままのブランチは last_conn_at が進まないため、
		// 現在の接続数を見て使用中なら停止・削除の対象から外す。
		if m.activeConns(b.Name) > 0 {
			continue
		}
		activity := b.CreatedAt
		if b.LastConnAt != nil && b.LastConnAt.After(activity) {
			activity = *b.LastConnAt
		}
		idle := now.Sub(activity)

		if m.cfg.DeleteAfterIdle > 0 && idle >= m.cfg.DeleteAfterIdle &&
			(b.State == state.StateRunning || b.State == state.StateSleeping) {
			obs.Log(ctx).Info("reaper deleting idle branch", "component", "reaper", "branch", b.Name, "idle", idle.Round(time.Second).String())
			if err := m.Delete(ctx, b.Name); err != nil {
				obs.Log(ctx).Error("reaper delete failed", "component", "reaper", "branch", b.Name, "error", err.Error())
			}
			continue
		}
		if m.cfg.IdleStopAfter > 0 && idle >= m.cfg.IdleStopAfter && b.State == state.StateRunning {
			obs.Log(ctx).Info("reaper stopping idle branch", "component", "reaper", "branch", b.Name, "idle", idle.Round(time.Second).String())
			if err := m.Sleep(ctx, b.Name); err != nil {
				obs.Log(ctx).Error("reaper stop failed", "component", "reaper", "branch", b.Name, "error", err.Error())
			}
		}
	}
	return nil
}

// Sleep は mysqld を正常終了させて sleeping にする(データは残る、再開は Wake)。
func (m *Manager) Sleep(ctx context.Context, name string) error {
	unlock := m.lock(name)
	defer unlock()

	b, err := m.db.GetBranch(name)
	if err != nil {
		return err
	}
	if b.State != state.StateRunning {
		return nil
	}
	vol, err := m.resolveVolume(ctx, b)
	if err != nil {
		return err
	}
	if err := m.eng.Stop(ctx, m.instance(b, vol)); err != nil {
		return err
	}
	return m.db.SetState(name, state.StateSleeping, "")
}
