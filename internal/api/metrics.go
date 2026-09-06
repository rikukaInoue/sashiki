// Prometheus テキスト形式のメトリクス(外部依存なしの手書きエクスポジション)。
package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/rikukaInoue/sashiki/internal/obs"
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

		// capacity 由来のゲージ(memory headroom / pool usage)。仕様 14-1/14-2 で
		// admission が使うのと同じソース(Manager.Capacity)を再利用する。
		if capa, err := mgr.Capacity(r.Context()); err == nil {
			b.WriteString("# HELP sashiki_memory_available_bytes MemAvailable observed by admission\n# TYPE sashiki_memory_available_bytes gauge\n")
			fmt.Fprintf(&b, "sashiki_memory_available_bytes %d\n", capa.MemAvailableBytes)
			b.WriteString("# HELP sashiki_memory_expected_rss_bytes Expected RSS per mysqld used by admission\n# TYPE sashiki_memory_expected_rss_bytes gauge\n")
			fmt.Fprintf(&b, "sashiki_memory_expected_rss_bytes %d\n", capa.ExpectedRSSBytes)
			// headroom = MemAvailable が次の 1 本(expected_rss)を admit しても
			// なお残る空き。負なら admission が拒否する水準。
			headroom := capa.MemAvailableBytes - capa.ExpectedRSSBytes
			b.WriteString("# HELP sashiki_memory_headroom_bytes MemAvailable minus expected RSS of one more mysqld\n# TYPE sashiki_memory_headroom_bytes gauge\n")
			fmt.Fprintf(&b, "sashiki_memory_headroom_bytes %d\n", headroom)

			if capa.StorageIntrospectable {
				b.WriteString("# HELP sashiki_pool_used_bytes Storage pool used bytes\n# TYPE sashiki_pool_used_bytes gauge\n")
				fmt.Fprintf(&b, "sashiki_pool_used_bytes %d\n", capa.PoolUsedBytes)
				b.WriteString("# HELP sashiki_pool_total_bytes Storage pool total bytes\n# TYPE sashiki_pool_total_bytes gauge\n")
				fmt.Fprintf(&b, "sashiki_pool_total_bytes %d\n", capa.PoolTotalBytes)
				b.WriteString("# HELP sashiki_pool_used_ratio Storage pool used ratio (0-1)\n# TYPE sashiki_pool_used_ratio gauge\n")
				fmt.Fprintf(&b, "sashiki_pool_used_ratio %g\n", capa.PoolUsedRatio)
			}
			b.WriteString("# HELP sashiki_running_branches Number of running mysqld (memory-bearing)\n# TYPE sashiki_running_branches gauge\n")
			fmt.Fprintf(&b, "sashiki_running_branches %d\n", capa.Running)
		}

		// operation 数 / 所要時間 / hook 失敗数(obs が時間軸で集計)。
		obs.WriteMetrics(&b)

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(b.String()))
	})
}
