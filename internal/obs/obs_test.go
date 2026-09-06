package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// operation_id / branch が JSON ログ 1 行に必ず乗ることを検証する(仕様 20-5)。
func TestLogCarriesOperationIDAndBranch(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	t.Cleanup(func() { SetOutput(bytesDiscard{}) })

	ctx := WithOperation(context.Background(), "op_abc123", "pr-42")
	Log(ctx).Info("operation started", "type", "create")

	line := strings.TrimSpace(buf.String())
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, line)
	}
	if m["operation_id"] != "op_abc123" {
		t.Errorf("operation_id = %v, want op_abc123", m["operation_id"])
	}
	if m["branch"] != "pr-42" {
		t.Errorf("branch = %v, want pr-42", m["branch"])
	}
	if m["type"] != "create" {
		t.Errorf("type = %v, want create", m["type"])
	}
	if m["msg"] != "operation started" {
		t.Errorf("msg = %v", m["msg"])
	}
}

// operation の対象が branch でない(空)場合でも operation_id は乗り、branch は出さない。
func TestLogOmitsEmptyBranch(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	t.Cleanup(func() { SetOutput(bytesDiscard{}) })

	ctx := WithOperation(context.Background(), "op_x", "")
	Log(ctx).Info("hi")
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatal(err)
	}
	if m["operation_id"] != "op_x" {
		t.Errorf("operation_id = %v", m["operation_id"])
	}
	if _, ok := m["branch"]; ok {
		t.Errorf("branch should be absent, got %v", m["branch"])
	}
}

func TestWriteMetricsOutput(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	RecordOperation("create", "completed", 1500*time.Millisecond)
	RecordOperation("create", "failed", 500*time.Millisecond)
	RecordOperation("reset", "completed", 200*time.Millisecond)
	RecordHookFailure("on-create")
	RecordHookFailure("on-create")

	var buf bytes.Buffer
	WriteMetrics(&buf)
	out := buf.String()

	wants := []string{
		"# TYPE sashiki_operations_total counter",
		`sashiki_operations_total{type="create",result="completed"} 1`,
		`sashiki_operations_total{type="create",result="failed"} 1`,
		`sashiki_operations_total{type="reset",result="completed"} 1`,
		"# TYPE sashiki_operation_duration_seconds summary",
		`sashiki_operation_duration_seconds_count{type="create"} 2`,
		`sashiki_operation_duration_seconds_sum{type="create"} 2`,
		`sashiki_operation_duration_seconds_count{type="reset"} 1`,
		"# TYPE sashiki_hook_failures_total counter",
		`sashiki_hook_failures_total{event="on-create"} 2`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("metrics output missing %q\n---\n%s", w, out)
		}
	}
}

type bytesDiscard struct{}

func (bytesDiscard) Write(p []byte) (int, error) { return len(p), nil }
