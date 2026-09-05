// Prometheus テキスト形式のメトリクス(外部依存なしの手書きエクスポジション)。
package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/rikukaInoue/twig/internal/branch"
)

// MetricsHandler は GET /metrics を返すハンドラ。
func MetricsHandler(mgr *branch.Manager) http.Handler {
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
		b.WriteString("# HELP twig_branches Number of branches by state\n# TYPE twig_branches gauge\n")
		for _, st := range []string{"creating", "running", "sleeping", "resetting", "deleting", "error"} {
			fmt.Fprintf(&b, "twig_branches{state=%q} %d\n", st, byState[st])
		}
		b.WriteString("# HELP twig_branch_used_bytes CoW usage per branch\n# TYPE twig_branch_used_bytes gauge\n")
		for _, i := range infos {
			fmt.Fprintf(&b, "twig_branch_used_bytes{branch=%q} %d\n", i.Name, i.UsedBytes)
		}
		b.WriteString("# HELP twig_used_bytes_total Total CoW usage of all branches\n# TYPE twig_used_bytes_total gauge\n")
		fmt.Fprintf(&b, "twig_used_bytes_total %d\n", totalUsed)
		b.WriteString("# HELP twig_baseline_refreshing 1 while a baseline refresh is running\n# TYPE twig_baseline_refreshing gauge\n")
		refreshing := 0
		if branch.RefreshInProgress() {
			refreshing = 1
		}
		fmt.Fprintf(&b, "twig_baseline_refreshing %d\n", refreshing)

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(b.String()))
	})
}
