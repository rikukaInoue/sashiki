// ベースライン更新(仕様 10-5)。refresh スクリプト(データ投入・マスク・
// マイグレーション適用・mysqld 正常終了までを担当)を実行し、完了後に
// 新しい baseline snapshot を取得して current を切り替える。
// 古い baseline から生えた既存ブランチには影響しない。
//
// 不変条件「スナップショットは必ず正常終了状態でのみ取得する」の担保は
// スクリプトの exit 0 だけに頼らず、snapshot 取得前に base の datadir を
// 掴んでいるプロセスが残っていないことを検証する(quiesce チェック)。
package branch

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrRefreshRunning は refresh の多重実行。
var ErrRefreshRunning = fmt.Errorf("baseline refresh is already running")

// RefreshConfig は refresh の設定。
type RefreshConfig struct {
	Script  string        // /etc/twig/refresh.sh
	Timeout time.Duration // 既定 1h
	// CheckQuiesced は snapshot 取得前の検証(テストで注入)。nil なら
	// storage の BasePath から既定実装を組み立てる。
	CheckQuiesced func(ctx context.Context) error
}

// basePathProvider は base の実パスを返せるバックエンド(ebszfs)。
type basePathProvider interface {
	BasePath(ctx context.Context) (string, error)
}

var (
	refreshRunning atomic.Bool
	refreshLastErr atomic.Value // string
)

// RefreshLastError は直近の refresh 失敗理由(成功時は空)。
func RefreshLastError() string {
	if v := refreshLastErr.Load(); v != nil {
		return v.(string)
	}
	return ""
}

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
	if rc.CheckQuiesced == nil {
		rc.CheckQuiesced = m.defaultQuiesceCheck
	}
	if !refreshRunning.CompareAndSwap(false, true) {
		return "", ErrRefreshRunning
	}
	tag = "baseline-" + time.Now().UTC().Format("20060102T150405Z")

	go func() {
		defer refreshRunning.Store(false)
		cctx, cancel := context.WithTimeout(context.Background(), rc.Timeout)
		defer cancel()
		if err := m.runRefresh(cctx, rc, tag); err != nil {
			refreshLastErr.Store(err.Error())
			log.Printf("baseline refresh: %v", err)
			return
		}
		refreshLastErr.Store("")
	}()
	return tag, nil
}

func (m *Manager) runRefresh(ctx context.Context, rc RefreshConfig, tag string) error {
	cmd := exec.CommandContext(ctx, rc.Script)
	cmd.Env = append(os.Environ(),
		"TWIG_EVENT=baseline-refresh",
		"TWIG_BASELINE_TAG="+tag,
	)
	out, scriptErr := cmd.CombinedOutput()
	if scriptErr != nil {
		// スクリプト失敗時も mysqld が残っていれば回収を試みる(自己修復)
		if qerr := rc.CheckQuiesced(ctx); qerr != nil {
			m.reclaimBase(ctx)
		}
		return fmt.Errorf("script failed: %w: %s", scriptErr, tail(out, 500))
	}
	// exit 0 でも信用せず、snapshot 取得前に quiesce を検証する
	if err := rc.CheckQuiesced(ctx); err != nil {
		m.reclaimBase(ctx)
		if err2 := rc.CheckQuiesced(ctx); err2 != nil {
			return fmt.Errorf("base is not quiesced after script (snapshot aborted): %w", err2)
		}
		log.Printf("baseline refresh: leftover mysqld was terminated before snapshot")
	}
	snap, err := m.st.SnapshotBase(ctx, tag)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := m.db.SetCurrentBaseline(string(snap)); err != nil {
		return fmt.Errorf("set current: %w", err)
	}
	log.Printf("baseline refresh: current is now %s", snap)
	return nil
}

// defaultQuiesceCheck は base の datadir を引数に持つプロセスが残っていないか
// を確認する。BasePath を提供しないバックエンド(fsx)ではスキップ。
func (m *Manager) defaultQuiesceCheck(ctx context.Context) error {
	bp, ok := m.st.(basePathProvider)
	if !ok {
		return nil
	}
	path, err := bp.BasePath(ctx)
	if err != nil || path == "" {
		return nil
	}
	pids := findProcsUsing(path)
	if len(pids) > 0 {
		return fmt.Errorf("processes still using %s: pids %v", path, pids)
	}
	return nil
}

// reclaimBase は base の datadir を掴んでいるプロセスへ SIGTERM を送り、
// 停止を待つ(mysqld は TERM で正常シャットダウンする)。
func (m *Manager) reclaimBase(ctx context.Context) {
	bp, ok := m.st.(basePathProvider)
	if !ok {
		return
	}
	path, err := bp.BasePath(ctx)
	if err != nil || path == "" {
		return
	}
	pids := findProcsUsing(path)
	for _, pid := range pids {
		log.Printf("baseline refresh: sending SIGTERM to leftover pid %d", pid)
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(findProcsUsing(path)) == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// findProcsUsing は path をコマンドラインに含むプロセスの PID 一覧(pgrep -f)。
func findProcsUsing(path string) []int {
	out, err := exec.Command("pgrep", "-f", "--", "datadir="+path).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// RefreshInProgress は実行中かどうか。
func RefreshInProgress() bool { return refreshRunning.Load() }

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "..." + string(b[len(b)-n:])
}
