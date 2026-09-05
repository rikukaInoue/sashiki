package ops

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rikukaInoue/sashiki/internal/state"
)

func newStore(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRunSyncSuccess(t *testing.T) {
	r := New(newStore(t))
	id, err := r.RunSync("create", "pr-1", func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	op, err := r.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != state.OpCompleted || op.Type != "create" || op.Target != "pr-1" {
		t.Errorf("op = %+v", op)
	}
	if op.FinishedAt == nil {
		t.Error("finished_at should be set")
	}
}

func TestRunSyncFailureRecordsError(t *testing.T) {
	r := New(newStore(t))
	id, err := r.RunSync("delete", "pr-1", func(context.Context) error { return errors.New("boom") })
	if err == nil {
		t.Fatal("want error")
	}
	op, _ := r.Get(id)
	if op.State != state.OpFailed || op.Error != "boom" {
		t.Errorf("op = %+v", op)
	}
}

func TestStartIsAsyncAndWaitObservesCompletion(t *testing.T) {
	r := New(newStore(t))
	release := make(chan struct{})
	id, err := r.Start("reset", "pr-1", func(context.Context) error {
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// まだ running
	op, _ := r.Get(id)
	if op.State != state.OpRunning {
		t.Errorf("state = %s, want running", op.State)
	}
	close(release)
	op, err = r.Wait(context.Background(), id, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != state.OpCompleted {
		t.Errorf("state = %s, want completed", op.State)
	}
}

func TestListNewestFirst(t *testing.T) {
	r := New(newStore(t))
	_, _ = r.RunSync("create", "pr-1", func(context.Context) error { return nil })
	time.Sleep(2 * time.Millisecond)
	_, _ = r.RunSync("delete", "pr-1", func(context.Context) error { return nil })
	list, err := r.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Type != "delete" {
		t.Errorf("list = %+v (newest should be first)", list)
	}
}

var errSentinel = errors.New("sentinel")

func TestRunSyncPreservesErrorType(t *testing.T) {
	r := New(newStore(t))
	_, err := r.RunSync("create", "pr-1", func(context.Context) error { return errSentinel })
	if !errors.Is(err, errSentinel) {
		t.Errorf("RunSync should return the original error unwrapped: %v", err)
	}
}
