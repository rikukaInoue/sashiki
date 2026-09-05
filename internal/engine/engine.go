// Package engine は DB エンジン依存の操作(起動・停止・ready 判定)を閉じ込める。
// Postgres 対応時はこのインターフェースの実装を1つ足すだけにする。
package engine

import "context"

// Instance はブランチ1つ分の DB プロセスの識別情報。
type Instance struct {
	Branch  string
	DataDir string // <volume path>/data
	Port    int
}

// Engine は DB エンジンが実装するインターフェース。
type Engine interface {
	// Start はインスタンスを起動する(非同期でよい)。
	Start(ctx context.Context, ins Instance) error
	// Stop は正常終了させる。@init/@baseline の一貫性はこれに依存する。
	Stop(ctx context.Context, ins Instance) error
	// WaitReady は接続可能になるまで待つ。
	WaitReady(ctx context.Context, ins Instance) error
	// IsRunning は起動中かどうか。
	IsRunning(ctx context.Context, ins Instance) (bool, error)
}
