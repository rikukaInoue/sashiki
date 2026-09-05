// Prometheus テキスト形式のメトリクス(外部依存なしの手書きエクスポジション)。
package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/rikukaInoue/sashiki/internal/workspace"
)

// MetricsHandler は GET /metrics を返すハンドラ。
// per-branch ラベル(sashiki_branch_used_bytes)のカーディナリティは
// branches.max_branches(既定50)で有界。上限を大きく上げる運用では
// Prometheus 側の series 数に注意。
func MetricsHandler(mgr *workspace.Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		infos, err := mgr.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var b strings.Builder
		byState := map[string]int{}
		var totalUsed int64
		for _, i := range infos {
			byState[i.State]++
			totalUsed += i.UsedBytes
		}
		b.WriteString("# HELP sashiki_branches Number of branches by state\n# TYPE sashiki_branches gauge\n")
		for _, st := range []string{"creating", "running", "sleeping", "resetting", "deleting", "error"} {
			fmt.Fprintf(&b, "sashiki_branches{state=%q} %d\n", st, byState[st])
		}
		b.WriteString("# HELP sashiki_branch_used_bytes CoW usage per branch\n# TYPE sashiki_branch_used_bytes gauge\n")
		for _, i := range infos {
			fmt.Fprintf(&b, "sashiki_branch_used_bytes{branch=%q} %d\n", i.Name, i.UsedBytes)
		}
		b.WriteString("# HELP sashiki_used_bytes_total Total CoW usage of all branches\n# TYPE sashiki_used_bytes_total gauge\n")
		fmt.Fprintf(&b, "sashiki_used_bytes_total %d\n", totalUsed)
		b.WriteString("# HELP sashiki_baseline_refreshing 1 while a baseline refresh is running\n# TYPE sashiki_baseline_refreshing gauge\n")
		refreshing := 0
		if workspace.RefreshInProgress() {
			refreshing = 1
		}
		fmt.Fprintf(&b, "sashiki_baseline_refreshing %d\n", refreshing)

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(b.String()))
	})
}
