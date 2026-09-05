// ベースライン更新(仕様 10-5)。refresh スクリプト(データ投入・マスク・
// マイグレーション適用・mysqld 正常終了までを担当)を実行し、完了後に
// 新しい baseline snapshot を取得して current を切り替える。
// 古い baseline から生えた既存ブランチには影響しない。
//
// 不変条件「スナップショットは必ず正常終了状態でのみ取得する」の担保は
// スクリプトの exit 0 だけに頼らず、snapshot 取得前に base の datadir を
// 掴んでいるプロセスが残っていないことを検証する(quiesce チェック)。
package workspace

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

	"github.com/rikukaInoue/sashiki/internal/hooks"
	"github.com/rikukaInoue/sashiki/internal/state"
	"github.com/rikukaInoue/sashiki/internal/storage"
)

// ErrRefreshRunning は refresh の多重実行。
var ErrRefreshRunning = fmt.Errorf("baseline refresh is already running")

// RefreshConfig は refresh の設定。
type RefreshConfig struct {
	Script  string        // /etc/sashiki/refresh.sh
	Timeout time.Duration // 既定 1h
	// CheckQuiesced は snapshot 取得前の検証(テストで注入)。nil なら
	// storage の BasePath から既定実装を組み立てる。
	CheckQuiesced func(ctx context.Context) error

	// publish ポリシー(仕様 12-4)。
	RequireMasked    bool
	RequireValidated bool
	ValidatePort     int
	MaskedSentinel   string
	// SkipValidate はテストで validate(clone+engine)を飛ばす。
	SkipValidate bool
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
		rc.Script = "/etc/sashiki/refresh.sh"
	}
	if rc.Timeout == 0 {
		rc.Timeout = time.Hour
	}
	// ポリシー未指定なら Manager 既定を使う。
	if !rc.RequireMasked && m.baselinePolicy.RequireMasked {
		rc.RequireMasked = true
	}
	if !rc.RequireValidated && m.baselinePolicy.RequireValidated {
		rc.RequireValidated = true
	}
	if rc.ValidatePort == 0 {
		rc.ValidatePort = m.baselinePolicy.ValidatePort
	}
	if rc.MaskedSentinel == "" {
		rc.MaskedSentinel = m.baselinePolicy.MaskedSentinel
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
		"SASHIKI_EVENT=baseline-refresh",
		"SASHIKI_BASELINE_TAG="+tag,
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
	// --- build: script が投入/マスク/migration/正常終了を済ませた candidate を snapshot ---
	snap, err := m.st.SnapshotBase(ctx, tag)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	masked := rc.MaskedSentinel != "" && fileExists(rc.MaskedSentinel)
	prov := state.BaselineProvenance{DataAsOf: tag, Masked: masked}
	if err := m.db.RegisterBaseline(string(snap), prov); err != nil {
		return fmt.Errorf("register candidate: %w", err)
	}

	// --- validate: candidate を一時 branch で起動して検証 → 破棄 ---
	validated := false
	if !rc.SkipValidate {
		if err := m.validateCandidate(ctx, snap, rc); err != nil {
			return fmt.Errorf("validate: %w (candidate %s は publish しない)", err, snap)
		}
		validated = true
		prov.Validated = true
		_ = m.db.RegisterBaseline(string(snap), prov)
	}

	// --- publish: ポリシーを満たせば current pointer を candidate へ ---
	if rc.RequireMasked && !masked {
		return fmt.Errorf("publish rejected: baseline is not masked (require_masked)")
	}
	if rc.RequireValidated && !validated {
		return fmt.Errorf("publish rejected: baseline is not validated (require_validated)")
	}
	if err := m.db.SetCurrentBaseline(string(snap)); err != nil {
		return fmt.Errorf("publish (set current): %w", err)
	}
	log.Printf("baseline refresh: published %s (masked=%v validated=%v)", snap, masked, validated)
	return nil
}

// validateCandidate は candidate snapshot を一時 branch で起動し、
// on-baseline-validate hook で検証してから破棄する(仕様 12-4)。
// crash recovery が走らずに起動できること自体が「正常終了状態で撮られた」検証を兼ねる。
func (m *Manager) validateCandidate(ctx context.Context, snap storage.SnapshotRef, rc RefreshConfig) error {
	name := "_validate"
	// 既存の検証 volume が残っていれば掃除
	if vol, err := m.resolveVolume(ctx, state.Branch{Name: name}); err == nil {
		_ = m.eng.Kill(ctx, m.instance(state.Branch{Name: name, Port: rc.ValidatePort}, vol))
		if job, derr := m.st.DeleteAsync(ctx, vol); derr == nil {
			_, _ = m.st.Poll(ctx, job)
		}
	}
	vol, err := m.st.Clone(ctx, snap, name)
	if err != nil {
		return fmt.Errorf("clone candidate: %w", err)
	}
	port := rc.ValidatePort
	if port == 0 {
		port = 3999
	}
	b := state.Branch{Name: name, Port: port}
	ins := m.instance(b, vol)
	cleanup := func() {
		_ = m.eng.Kill(ctx, ins)
		if job, derr := m.st.DeleteAsync(ctx, vol); derr == nil {
			_, _ = m.st.Poll(ctx, job)
		}
	}
	defer cleanup()

	// crash recovery なしで ready になること = 正常終了状態で撮られた証拠。
	if err := m.eng.Start(ctx, ins); err != nil {
		return fmt.Errorf("candidate engine start: %w", err)
	}
	if err := m.eng.WaitReady(ctx, ins); err != nil {
		return fmt.Errorf("candidate not ready (crash recovery?): %w", err)
	}
	// operator の検証(mask validation / migration version / sanity)。無ければスキップ。
	if m.hooks != nil {
		if _, ok := m.hooks.Find(hooks.OnBaselineValidate); ok {
			if err := m.runHook(ctx, hooks.OnBaselineValidate, b, vol); err != nil {
				return fmt.Errorf("validation hook failed: %w", err)
			}
		}
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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
