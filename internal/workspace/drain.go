// drain: 全 running branch を正常終了(sleeping)させる(仕様 17章 / #28)。
// instance_class 変更や計画メンテの前に、mysqld を安全に落としてから作業する
// ために使う。データは残り、再接続 / Wake で復帰する(reaper の Sleep と同じ)。
package workspace

import (
	"context"

	"github.com/rikukadev/sashiki/internal/state"
)

// DrainResult は drain の結果。
type DrainResult struct {
	Slept   []string          // 今回停止した branch
	Skipped []string          // 既に running でない(sleeping/error 等)
	Failed  map[string]string // 停止に失敗した branch → 理由
}

// Drain は全 running branch を正常終了させて sleeping にする。
// 個々の失敗は Failed に集約し、途中で止めずに全 branch を試みる。
func (m *Manager) Drain(ctx context.Context) (DrainResult, error) {
	branches, err := m.db.ListBranches()
	if err != nil {
		return DrainResult{}, err
	}
	res := DrainResult{Failed: map[string]string{}}
	for _, b := range branches {
		if b.State != state.StateRunning {
			res.Skipped = append(res.Skipped, b.Name)
			continue
		}
		if err := m.Sleep(ctx, b.Name); err != nil {
			res.Failed[b.Name] = err.Error()
			continue
		}
		res.Slept = append(res.Slept, b.Name)
	}
	return res, nil
}
