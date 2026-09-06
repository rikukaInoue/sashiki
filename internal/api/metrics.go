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

		// --- capacity(仕様 20-5: pool usage / memory headroom)---
		if c, err := mgr.Capacity(r.Context()); err == nil {
			if c.StorageIntrospectable {
				b.WriteString("# HELP sashiki_pool_used_bytes Pool used bytes\n# TYPE sashiki_pool_used_bytes gauge\n")
				fmt.Fprintf(&b, "sashiki_pool_used_bytes %d\n", c.PoolUsedBytes)
				b.WriteString("# HELP sashiki_pool_total_bytes Pool total bytes\n# TYPE sashiki_pool_total_bytes gauge\n")
				fmt.Fprintf(&b, "sashiki_pool_total_bytes %d\n", c.PoolTotalBytes)
				b.WriteString("# HELP sashiki_pool_used_ratio Pool usage ratio (0-1)\n# TYPE sashiki_pool_used_ratio gauge\n")
				fmt.Fprintf(&b, "sashiki_pool_used_ratio %g\n", c.PoolUsedRatio)
			}
			b.WriteString("# HELP sashiki_storage_watermark Configured watermarks (0-1, 0 = disabled)\n# TYPE sashiki_storage_watermark gauge\n")
			fmt.Fprintf(&b, "sashiki_storage_watermark{level=\"high\"} %g\n", c.HighWatermark)
			fmt.Fprintf(&b, "sashiki_storage_watermark{level=\"critical\"} %g\n", c.CritWatermark)
			b.WriteString("# HELP sashiki_memory_available_bytes MemAvailable at scrape time\n# TYPE sashiki_memory_available_bytes gauge\n")
			fmt.Fprintf(&b, "sashiki_memory_available_bytes %d\n", c.MemAvailableBytes)
			b.WriteString("# HELP sashiki_memory_expected_rss_bytes Expected RSS per running instance\n# TYPE sashiki_memory_expected_rss_bytes gauge\n")
			fmt.Fprintf(&b, "sashiki_memory_expected_rss_bytes %d\n", c.ExpectedRSSBytes)
			b.WriteString("# HELP sashiki_max_running Configured max concurrently running instances (0 = unlimited)\n# TYPE sashiki_max_running gauge\n")
			fmt.Fprintf(&b, "sashiki_max_running %d\n", c.MaxRunning)
			b.WriteString("# HELP sashiki_ports_used Allocated ports\n# TYPE sashiki_ports_used gauge\n")
			fmt.Fprintf(&b, "sashiki_ports_used %d\n", c.PortsUsed)
			b.WriteString("# HELP sashiki_ports_total Ports in port_range\n# TYPE sashiki_ports_total gauge\n")
			fmt.Fprintf(&b, "sashiki_ports_total %d\n", c.PortsTotal)
		}

		// --- operations(仕様 20-5: operation 数と所要時間)---
		if stats, err := mgr.OperationStats(); err == nil {
			b.WriteString("# HELP sashiki_operations_total Operations by type and state\n# TYPE sashiki_operations_total counter\n")
			for _, s := range stats {
				fmt.Fprintf(&b, "sashiki_operations_total{type=%q,state=%q} %d\n", s.Type, s.State, s.Count)
			}
			b.WriteString("# HELP sashiki_operation_duration_seconds Total duration of finished operations\n# TYPE sashiki_operation_duration_seconds counter\n")
			for _, s := range stats {
				if s.State == "running" {
					continue
				}
				fmt.Fprintf(&b, "sashiki_operation_duration_seconds_sum{type=%q,state=%q} %g\n", s.Type, s.State, s.DurationSum)
				fmt.Fprintf(&b, "sashiki_operation_duration_seconds_count{type=%q,state=%q} %d\n", s.Type, s.State, s.Count)
			}
		}

		// --- hooks(仕様 20-5: hook 失敗数)---
		if n, err := mgr.HookFailureCount(); err == nil {
			b.WriteString("# HELP sashiki_hook_failures_total Hook runs with non-zero exit code\n# TYPE sashiki_hook_failures_total counter\n")
			fmt.Fprintf(&b, "sashiki_hook_failures_total %d\n", n)
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(b.String()))
	})
}
