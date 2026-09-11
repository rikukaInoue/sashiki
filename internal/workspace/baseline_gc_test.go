package workspace

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rikukadev/sashiki/internal/state"
)

func TestGCBaselinesKeepLastRetentionDryRun(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	for _, tag := range []string{"a", "b", "c", "d"} {
		if err := m.db.RegisterBaseline("pool/base@bl-"+tag, state.BaselineProvenance{}); err != nil {
			t.Fatal(err)
		}
	}
	// dry-run keep_last=1: 削除対象は挙がるが実際には消えない
	res, err := m.GCBaselines(context.Background(), GCConfig{KeepLast: 1, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) == 0 {
		t.Error("dry-run should list deletion targets")
	}
	if rows, _ := m.ListBaselineRows(); len(rows) != 4 {
		t.Errorf("dry-run must not delete: %d rows, want 4", len(rows))
	}
	// retention=1h: 全部新しいので消えない
	res, _ = m.GCBaselines(context.Background(), GCConfig{KeepLast: 1, Retention: time.Hour})
	if len(res.Deleted) != 0 {
		t.Errorf("retention should keep young baselines, deleted=%v", res.Deleted)
	}
	// 本番 keep_last=1: 実際に削除される
	if _, err := m.GCBaselines(context.Background(), GCConfig{KeepLast: 1}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := m.ListBaselineRows(); len(rows) != 1 {
		t.Errorf("after gc keep_last=1: %d rows, want 1", len(rows))
	}
}

func TestValidateAndDeleteBaseline(t *testing.T) {
	m := newTestManager(t, &mockStorage{}, &mockEngine{}, "")
	snap := "pool/base@bl-1"
	if err := m.db.RegisterBaseline(snap, state.BaselineProvenance{}); err != nil {
		t.Fatal(err)
	}
	// validate → validated=true
	if err := m.ValidateBaseline(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	validated := false
	rows, _ := m.ListBaselineRows()
	for _, r := range rows {
		if r.Snapshot == snap {
			validated = r.Prov.Validated
		}
	}
	if !validated {
		t.Error("baseline should be marked validated")
	}
	if err := m.ValidateBaseline(context.Background(), "nope"); !errors.Is(err, ErrBaselineNotFound) {
		t.Errorf("validate unknown err = %v, want ErrBaselineNotFound", err)
	}
	// delete candidate(current でない・参照なし)
	if err := m.DeleteBaseline(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.GetBaseline(snap); err == nil {
		t.Error("baseline should be deleted")
	}
	// current は削除拒否(precondition failed)
	cur := string(m.currentBaseline())
	_ = m.db.RegisterBaseline(cur, state.BaselineProvenance{})
	_ = m.db.SetCurrentBaseline(cur)
	if err := m.DeleteBaseline(context.Background(), cur); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("deleting current should be precondition-failed, got %v", err)
	}
}
