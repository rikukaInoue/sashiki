// Package ops は変更操作を非同期 operation として実行・追跡する(仕様 17章/19章)。
// すべての変更操作は 202 + operation を返す。ebs-zfs で 1 秒で終わっても
// 同じ形にする(fsx-zfs で数分かかる操作のため)。CLI は --wait でポーリングする。
package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/rikukaInoue/sashiki/internal/state"
)

// Store は operation の永続化(state.DB が実装)。
type Store interface {
	CreateOperation(id, typ, target string) error
	FinishOperation(id, errMsg string) error
	GetOperation(id string) (state.Operation, error)
	ListOperations(limit int) ([]state.Operation, error)
}

// Runner は operation の起動と追跡を担う。
type Runner struct {
	store Store
	// newID はテストで固定するための ID 生成器。
	newID func() string
	now   func() time.Time
}

// New は Runner を作る。
func New(store Store) *Runner {
	return &Runner{store: store, newID: randomID, now: time.Now}
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "op_" + hex.EncodeToString(b)
}

// Start は typ/target の operation を作り、fn をバックグラウンドで実行する。
// operation id を即座に返す(呼び出し側は 202 で返す)。
func (r *Runner) Start(typ, target string, fn func(ctx context.Context) error) (string, error) {
	id := r.newID()
	if err := r.store.CreateOperation(id, typ, target); err != nil {
		return "", err
	}
	go func() {
		// operation はリクエストのライフサイクルから切り離す(fsx は数分かかる)。
		ctx := context.Background()
		errMsg := ""
		if err := fn(ctx); err != nil {
			errMsg = err.Error()
		}
		_ = r.store.FinishOperation(id, errMsg)
	}()
	return id, nil
}

// RunSync は fn を同期実行しつつ operation として記録する(テスト・内部用)。
// fn のエラーはラップせずそのまま返す(errors.Is での型判定を壊さないため)。
func (r *Runner) RunSync(typ, target string, fn func(ctx context.Context) error) (string, error) {
	id := r.newID()
	if err := r.store.CreateOperation(id, typ, target); err != nil {
		return "", err
	}
	runErr := fn(context.Background())
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
	}
	_ = r.store.FinishOperation(id, errMsg)
	return id, runErr
}

// Get は operation を取得する。
func (r *Runner) Get(id string) (state.Operation, error) {
	return r.store.GetOperation(id)
}

// List は最近の operation を返す。
func (r *Runner) List(limit int) ([]state.Operation, error) {
	return r.store.ListOperations(limit)
}

// Wait は operation が完了(completed/failed)するまでポーリングして返す。
func (r *Runner) Wait(ctx context.Context, id string, poll time.Duration) (state.Operation, error) {
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	for {
		op, err := r.store.GetOperation(id)
		if err != nil {
			return op, err
		}
		if op.State != state.OpRunning {
			return op, nil
		}
		select {
		case <-ctx.Done():
			return op, ctx.Err()
		case <-time.After(poll):
		}
	}
}
