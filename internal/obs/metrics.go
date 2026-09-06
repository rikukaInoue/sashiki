// メトリクス集計(外部依存なしの手書き Prometheus エクスポジション)。
// gauge 系(memory headroom / pool usage / branch state)は都度 Manager から
// 算出するため api 層が書く。ここでは時間軸で積み上がるカウンタ/サマリ、
// すなわち operation 数・所要時間・hook 失敗数を保持する。
package obs

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

var mx = &metrics{
	operations: map[opKey]int64{},
	durCount:   map[string]int64{},
	durSum:     map[string]float64{},
	hookFails:  map[string]int64{},
}

type opKey struct {
	typ    string
	result string
}

type metrics struct {
	mu sync.Mutex
	// operations_total{type,result}
	operations map[opKey]int64
	// operation_duration_seconds{type} の count / sum(summary)
	durCount map[string]int64
	durSum   map[string]float64
	// hook_failures_total{event}
	hookFails map[string]int64
}

// RecordOperation は operation 1 件の完了を記録する(count と所要時間)。
// result は "completed" | "failed"。
func RecordOperation(typ, result string, dur time.Duration) {
	mx.mu.Lock()
	defer mx.mu.Unlock()
	mx.operations[opKey{typ: typ, result: result}]++
	mx.durCount[typ]++
	mx.durSum[typ] += dur.Seconds()
}

// RecordHookFailure は hook 実行失敗を記録する。
func RecordHookFailure(event string) {
	mx.mu.Lock()
	defer mx.mu.Unlock()
	mx.hookFails[event]++
}

// WriteMetrics は operation / hook 系のメトリクスを Prometheus テキスト形式で書く。
// 既存 gauge を書いたあとに api 層から呼ぶ(families を追記する)。
func WriteMetrics(w io.Writer) {
	mx.mu.Lock()
	// スナップショットを取ってからロックを離す(I/O をロック外で行う)。
	ops := make([]opKey, 0, len(mx.operations))
	opsVal := map[opKey]int64{}
	for k, v := range mx.operations {
		ops = append(ops, k)
		opsVal[k] = v
	}
	durTypes := make([]string, 0, len(mx.durCount))
	durCount := map[string]int64{}
	durSum := map[string]float64{}
	for k, v := range mx.durCount {
		durTypes = append(durTypes, k)
		durCount[k] = v
		durSum[k] = mx.durSum[k]
	}
	hooks := make([]string, 0, len(mx.hookFails))
	hookVal := map[string]int64{}
	for k, v := range mx.hookFails {
		hooks = append(hooks, k)
		hookVal[k] = v
	}
	mx.mu.Unlock()

	sort.Slice(ops, func(i, j int) bool {
		if ops[i].typ != ops[j].typ {
			return ops[i].typ < ops[j].typ
		}
		return ops[i].result < ops[j].result
	})
	sort.Strings(durTypes)
	sort.Strings(hooks)

	fmt.Fprint(w, "# HELP sashiki_operations_total Total operations by type and result\n# TYPE sashiki_operations_total counter\n")
	for _, k := range ops {
		fmt.Fprintf(w, "sashiki_operations_total{type=%q,result=%q} %d\n", k.typ, k.result, opsVal[k])
	}

	fmt.Fprint(w, "# HELP sashiki_operation_duration_seconds Operation wall-clock duration by type\n# TYPE sashiki_operation_duration_seconds summary\n")
	for _, t := range durTypes {
		fmt.Fprintf(w, "sashiki_operation_duration_seconds_count{type=%q} %d\n", t, durCount[t])
		fmt.Fprintf(w, "sashiki_operation_duration_seconds_sum{type=%q} %g\n", t, durSum[t])
	}

	fmt.Fprint(w, "# HELP sashiki_hook_failures_total Hook execution failures by event\n# TYPE sashiki_hook_failures_total counter\n")
	for _, e := range hooks {
		fmt.Fprintf(w, "sashiki_hook_failures_total{event=%q} %d\n", e, hookVal[e])
	}
}

// ResetForTest はテスト間で集計をクリアする。
func ResetForTest() {
	mx.mu.Lock()
	defer mx.mu.Unlock()
	mx.operations = map[opKey]int64{}
	mx.durCount = map[string]int64{}
	mx.durSum = map[string]float64{}
	mx.hookFails = map[string]int64{}
}
