// Package obs は observability(構造化ログ + メトリクス)を提供する。
// ログは log/slog の JSON handler を既定にし、operation_id / branch を
// context 経由で運んでどの操作のログかを常に辿れるようにする(仕様 20-5)。
package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
)

// logger はプロセス既定の構造化ロガー(JSON)。
var logger atomic.Pointer[slog.Logger]

func init() {
	logger.Store(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
}

// SetOutput は出力先を差し替える(テスト・起動時用)。
func SetOutput(w io.Writer) {
	logger.Store(slog.New(slog.NewJSONHandler(w, nil)))
}

// Logger は context を持たない場面用の既定ロガー。
func Logger() *slog.Logger { return logger.Load() }

// opCtxKey は operation フィールドを運ぶ context キー。
type opCtxKey struct{}

type opFields struct {
	id     string
	branch string
}

// WithOperation は operation_id / branch を context に載せる。
// branch が空(operation の対象が branch でない場合)でも id は運ぶ。
func WithOperation(ctx context.Context, operationID, branch string) context.Context {
	return context.WithValue(ctx, opCtxKey{}, opFields{id: operationID, branch: branch})
}

// Log は context に載った operation_id / branch を付与したロガーを返す。
// 何も載っていなければ既定ロガーをそのまま返す。
func Log(ctx context.Context) *slog.Logger {
	l := logger.Load()
	if ctx == nil {
		return l
	}
	f, ok := ctx.Value(opCtxKey{}).(opFields)
	if !ok {
		return l
	}
	if f.id != "" {
		l = l.With("operation_id", f.id)
	}
	if f.branch != "" {
		l = l.With("branch", f.branch)
	}
	return l
}
