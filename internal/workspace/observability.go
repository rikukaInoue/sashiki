// observability: metrics 用の集計を state.db から引く薄い委譲(仕様 20-5)。
// MetricsHandler が Manager 経由で取れるようにして、api 層が db を直接持たない
// 既存の構成を保つ。
package workspace

import "github.com/rikukadev/sashiki/internal/state"

// OperationStats は operations の type×state 集計を返す。
func (m *Manager) OperationStats() ([]state.OperationStat, error) {
	return m.db.OperationStats()
}

// HookFailureCount は失敗した hook 実行の累計を返す。
func (m *Manager) HookFailureCount() (int, error) {
	return m.db.HookFailureCount()
}
