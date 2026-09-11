// Package ops は変更操作を非同期 operation として実行・追跡する(仕様 17章/19章)。
// すべての変更操作は 202 + operation を返す。ebs-zfs で 1 秒で終わっても
// 同じ形にする(fsx-zfs で数分かかる操作のため)。CLI は --wait でポーリングする。
package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rikukadev/sashiki/internal/state"
)

// Store は operation の永続化(state.DB が実装)。
type Store interface {
	CreateOperation(id, typ, target string) error
	FinishOperation(id, errCode, errMsg string) error
	GetOperation(id string) (state.Operation, error)
	ListOperations(limit int) ([]state.Operation, error)
}

// errClassifier は失敗ステージ等の機械判定用コードを持つエラー(workspace の
// stageErr が実装)。import 循環を避けるため構造的に拾う。
type errClassifier interface{ Code() string }

// errInfo は runErr から error_code / message を取り出す(wrap されていても拾う)。
func errInfo(runErr error) (code, msg string) {
	if runErr == nil {
		return "", ""
	}
	var c errClassifier
	if errors.As(runErr, &c) {
		code = c.Code()
	}
	return code, runErr.Error()
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

// logOperation は operation の完了を構造化ログに残す(仕様 20-5:
// operation_id と branch を必ず含める)。
func (r *Runner) logOperation(id, typ, target string, start time.Time, err error) {
	attrs := []any{
		"operation_id", id,
		"type", typ,
		"branch", target,
		"duration_ms", r.now().Sub(start).Milliseconds(),
	}
	if err != nil {
		slog.Error("operation failed", append(attrs, "error", err.Error())...)
		return
	}
	slog.Info("operation completed", attrs...)
}

// Start は typ/target の operation を作り、fn をバックグラウンドで実行する。
// operation id を即座に返す(呼び出し側は 202 で返す)。
func (r *Runner) Start(typ, target string, fn func(ctx context.Context) error) (string, error) {
	id := r.newID()
	if err := r.store.CreateOperation(id, typ, target); err != nil {
		return "", err
	}
	start := r.now()
	go func() {
		// operation はリクエストのライフサイクルから切り離す(fsx は数分かかる)。
		ctx := context.Background()
		var runErr error
		// 非同期 op 内の panic でデーモンを落とさない(#53/#82)。
		func() {
			defer func() {
				if p := recover(); p != nil {
					runErr = fmt.Errorf("panic: %v", p)
				}
			}()
			runErr = fn(ctx)
		}()
		code, errMsg := errInfo(runErr)
		_ = r.store.FinishOperation(id, code, errMsg)
		r.logOperation(id, typ, target, start, runErr)
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
	start := r.now()
	runErr := fn(context.Background())
	code, errMsg := errInfo(runErr)
	_ = r.store.FinishOperation(id, code, errMsg)
	r.logOperation(id, typ, target, start, runErr)
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
