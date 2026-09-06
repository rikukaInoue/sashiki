package api

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rikukaInoue/sashiki/internal/obs"
	"github.com/rikukaInoue/sashiki/internal/state"
	"github.com/rikukaInoue/sashiki/internal/workspace"
)

func TestMetricsHandlerIncludesNewMetrics(t *testing.T) {
	obs.ResetForTest()
	t.Cleanup(obs.ResetForTest)

	// obs 集計に operation / hook 失敗を積む。
	obs.RecordOperation("create", "completed", 1200*time.Millisecond)
	obs.RecordOperation("reset", "failed", 300*time.Millisecond)
	obs.RecordHookFailure("on-create")

	db, err := state.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fs := fakeStorage{}
	mgr, err := workspace.New(workspace.Config{
		NamePattern: `^[a-z0-9-]{1,32}$`, MaxBranches: 10,
		PortLow: 3401, PortHigh: 3410, EngineType: "mysql", StateDir: t.TempDir(),
		// memory headroom ゲージを出すために admission ソースを与える。
		AvailableMem:     func() (int64, error) { return 8 * 1024 * 1024 * 1024, nil },
		ExpectedRSSBytes: 1 * 1024 * 1024 * 1024,
	}, fs, fs, fakeEngine{}, nil, db)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	MetricsHandler(mgr).ServeHTTP(rr, req)

	body := rr.Body.String()
	wants := []string{
		// 既存メトリクスは残っていること(回帰防止)。
		"sashiki_branches{state=",
		"sashiki_used_bytes_total",
		"sashiki_baseline_refreshing",
		// 追加: memory headroom / pool usage(memory 系は AvailableMem があるので出る)。
		"sashiki_memory_available_bytes",
		"sashiki_memory_expected_rss_bytes",
		"sashiki_memory_headroom_bytes",
		"sashiki_running_branches",
		// 追加: operation / hook メトリクス。
		`sashiki_operations_total{type="create",result="completed"} 1`,
		`sashiki_operations_total{type="reset",result="failed"} 1`,
		`sashiki_operation_duration_seconds_count{type="create"} 1`,
		`sashiki_operation_duration_seconds_sum{type="create"}`,
		`sashiki_hook_failures_total{event="on-create"} 1`,
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("metrics body missing %q\n---\n%s", w, body)
		}
	}

	// headroom = 8GiB - 1GiB = 7GiB。
	if !strings.Contains(body, "sashiki_memory_headroom_bytes 7516192768") {
		t.Errorf("unexpected headroom value\n%s", body)
	}
}
