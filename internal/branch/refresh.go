// ベースライン更新(仕様 10-5)。refresh スクリプト(データ投入・マスク・
// マイグレーション適用・mysqld 正常終了までを担当)を実行し、完了後に
// 新しい baseline snapshot を取得して current を切り替える。
// 古い baseline から生えた既存ブランチには影響しない。
package branch

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

// ErrRefreshRunning は refresh の多重実行。
var ErrRefreshRunning = fmt.Errorf("baseline refresh is already running")

// RefreshConfig は refresh の設定。
type RefreshConfig struct {
	Script  string        // /etc/twig/refresh.sh
	Timeout time.Duration // 既定 1h
}

var refreshRunning atomic.Bool

// RefreshBaseline は refresh を非同期で開始する(API は 202 を返す)。
// tag には baseline-YYYYMMDDHHMMSS を使う。
func (m *Manager) RefreshBaseline(ctx context.Context, rc RefreshConfig) (tag string, err error) {
	if rc.Script == "" {
		rc.Script = "/etc/twig/refresh.sh"
	}
	if rc.Timeout == 0 {
		rc.Timeout = time.Hour
	}
	if _, err := os.Stat(rc.Script); err != nil {
		return "", fmt.Errorf("refresh script %s: %w", rc.Script, err)
	}
	if !refreshRunning.CompareAndSwap(false, true) {
		return "", ErrRefreshRunning
	}
	tag = "baseline-" + time.Now().UTC().Format("20060102T150405Z")

	go func() {
		defer refreshRunning.Store(false)
		cctx, cancel := context.WithTimeout(context.Background(), rc.Timeout)
		defer cancel()

		cmd := exec.CommandContext(cctx, rc.Script)
		cmd.Env = append(os.Environ(),
			"TWIG_EVENT=baseline-refresh",
			"TWIG_BASELINE_TAG="+tag,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("baseline refresh: script failed: %v: %s", err, tail(out, 500))
			return
		}
		// スクリプトが mysqld を正常終了させた状態で snapshot を取得する
		// (不変条件: スナップショットは必ず正常終了状態でのみ取得する)。
		snap, err := m.st.SnapshotBase(cctx, tag)
		if err != nil {
			log.Printf("baseline refresh: snapshot: %v", err)
			return
		}
		if err := m.db.SetCurrentBaseline(string(snap)); err != nil {
			log.Printf("baseline refresh: set current: %v", err)
			return
		}
		log.Printf("baseline refresh: current is now %s", snap)
	}()
	return tag, nil
}

// RefreshInProgress は実行中かどうか。
func RefreshInProgress() bool { return refreshRunning.Load() }

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "..." + string(b[len(b)-n:])
}
