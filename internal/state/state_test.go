package state

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestBranchCRUD(t *testing.T) {
	db := openTest(t)
	if err := db.CreateBranch("pr-1", 3401, "pool/base@baseline"); err != nil {
		t.Fatal(err)
	}
	b, err := db.GetBranch("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != StateCreating || b.Port != 3401 {
		t.Errorf("branch = %+v", b)
	}
	if err := db.SetState("pr-1", StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	b, _ = db.GetBranch("pr-1")
	if b.State != StateRunning {
		t.Errorf("state = %s", b.State)
	}
	if err := db.DeleteBranch("pr-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetBranch("pr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestPortUniqueness(t *testing.T) {
	db := openTest(t)
	if err := db.CreateBranch("pr-1", 3401, "s"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBranch("pr-2", 3401, "s"); err == nil {
		t.Error("duplicate port should fail (UNIQUE constraint)")
	}
	used, err := db.UsedPorts()
	if err != nil {
		t.Fatal(err)
	}
	if !used[3401] || len(used) != 1 {
		t.Errorf("used = %v", used)
	}
}

func TestSetStateNotFound(t *testing.T) {
	db := openTest(t)
	if err := db.SetState("nope", StateRunning, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestHookRuns(t *testing.T) {
	db := openTest(t)
	if err := db.CreateBranch("pr-1", 3401, "s"); err != nil {
		t.Fatal(err)
	}
	id, err := db.RecordHookStart("pr-1", "on-create", "/log/1.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordHookFinish(id, 0); err != nil {
		t.Fatal(err)
	}
	id2, _ := db.RecordHookStart("pr-1", "on-reset", "/log/2.log")
	_ = db.RecordHookFinish(id2, 2)

	hs, err := db.LastHookStatus("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if hs["on-create"] != "ok" || hs["on-reset"] != "failed(2)" {
		t.Errorf("hook status = %v", hs)
	}
}

func TestTokens(t *testing.T) {
	db := openTest(t)
	if err := db.CreateToken("gha", "hash1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateToken("gha", "hash2"); err == nil {
		t.Error("duplicate name should fail")
	}
	ok, err := db.CheckTokenHash("hash1")
	if err != nil || !ok {
		t.Errorf("CheckTokenHash = %v, %v", ok, err)
	}
	if ok, _ := db.CheckTokenHash("nope"); ok {
		t.Error("unknown hash should be false")
	}
	tokens, _ := db.ListTokens()
	if len(tokens) != 1 || tokens[0].LastUsedAt == nil {
		t.Errorf("tokens = %+v (last_used should be set)", tokens)
	}
	if err := db.RevokeToken("gha"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.CheckTokenHash("hash1"); ok {
		t.Error("revoked token should be false")
	}
	if err := db.RevokeToken("gha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestOperationStatsAndHookFailureCount(t *testing.T) {
	db := openTest(t)

	// operations: create×2(completed/failed)、reset×1(running のまま)
	if err := db.CreateOperation("op_a", "create", "pr-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishOperation("op_a", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOperation("op_b", "create", "pr-2"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishOperation("op_b", "boom"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOperation("op_c", "reset", "pr-1"); err != nil {
		t.Fatal(err)
	}

	stats, err := db.OperationStats()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]OperationStat{}
	for _, s := range stats {
		got[s.Type+"/"+s.State] = s
	}
	if got["create/"+OpCompleted].Count != 1 || got["create/"+OpFailed].Count != 1 || got["reset/"+OpRunning].Count != 1 {
		t.Errorf("stats = %+v", got)
	}
	if got["create/"+OpCompleted].DurationSum < 0 {
		t.Errorf("duration should be non-negative: %+v", got["create/"+OpCompleted])
	}

	// hook_runs: 成功1・失敗1・未完了1 → 失敗のみカウント
	id1, _ := db.RecordHookStart("pr-1", "on-create", "/tmp/a.log")
	_ = db.RecordHookFinish(id1, 0)
	id2, _ := db.RecordHookStart("pr-1", "on-reset", "/tmp/b.log")
	_ = db.RecordHookFinish(id2, 1)
	_, _ = db.RecordHookStart("pr-2", "on-create", "/tmp/c.log")

	n, err := db.HookFailureCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("hook failures = %d, want 1", n)
	}
}

func TestOperationErrorJSONAndPrune(t *testing.T) {
	db := openTest(t)
	if err := db.CreateOperation("op-ok", "create", "pr-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishOperation("op-ok", ""); err != nil {
		t.Fatal(err)
	}
	// 失敗 op: error_json は JSON、Error は message
	if err := db.CreateOperation("op-bad", "create", "pr-2"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishOperation("op-bad", "clone failed: boom"); err != nil {
		t.Fatal(err)
	}
	bad, _ := db.GetOperation("op-bad")
	if bad.State != OpFailed || bad.Error != "clone failed: boom" {
		t.Errorf("op-bad = %+v", bad)
	}
	var raw string
	if err := db.sql.QueryRow(`SELECT error_json FROM operations WHERE id='op-bad'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		t.Errorf("error_json should be JSON, got %q", raw)
	}
	// 旧データ(生文字列)も message として読めること(後方互換)
	if _, err := db.sql.Exec(`UPDATE operations SET error_json='legacy raw' WHERE id='op-bad'`); err != nil {
		t.Fatal(err)
	}
	if leg, _ := db.GetOperation("op-bad"); leg.Error != "legacy raw" {
		t.Errorf("legacy error = %q, want 'legacy raw'", leg.Error)
	}

	// running op は prune 対象外
	if err := db.CreateOperation("op-run", "reset", "pr-3"); err != nil {
		t.Fatal(err)
	}
	n, err := db.PruneOperations(time.Now().Add(time.Hour)) // 未来 cutoff → finished 済みは全部
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("pruned = %d, want >= 2 (op-ok, op-bad)", n)
	}
	if _, err := db.GetOperation("op-ok"); !errors.Is(err, ErrNotFound) {
		t.Error("finished op-ok should be pruned")
	}
	if _, err := db.GetOperation("op-run"); err != nil {
		t.Errorf("running op must NOT be pruned: %v", err)
	}
}

func TestProvisionAndEngineState(t *testing.T) {
	db := openTest(t)
	if err := db.CreateBranch("pr-1", 3401, "p/base@baseline"); err != nil {
		t.Fatal(err)
	}

	// 実体参照の記録(#90)
	if err := db.SetProvision("pr-1", "p/branches/pr-1", "p/branches/pr-1@init"); err != nil {
		t.Fatal(err)
	}
	b, err := db.GetBranch("pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.VolumeRef != "p/branches/pr-1" || b.InitSnapshot != "p/branches/pr-1@init" {
		t.Errorf("provision = %q / %q", b.VolumeRef, b.InitSnapshot)
	}
	if err := db.SetProvision("nope", "x", "y"); err != ErrNotFound {
		t.Errorf("missing branch should be ErrNotFound, got %v", err)
	}

	// engine_state は lifecycle state から導出(running→running / sleeping→stopped、
	// 遷移中は前回値を保持)
	_ = db.SetState("pr-1", StateRunning, "")
	if b, _ = db.GetBranch("pr-1"); b.EngineState != "running" {
		t.Errorf("engine_state after running = %q", b.EngineState)
	}
	_ = db.SetState("pr-1", StateResetting, "")
	if b, _ = db.GetBranch("pr-1"); b.EngineState != "running" {
		t.Errorf("engine_state should be kept during resetting, got %q", b.EngineState)
	}
	_ = db.SetState("pr-1", StateSleeping, "")
	if b, _ = db.GetBranch("pr-1"); b.EngineState != "stopped" {
		t.Errorf("engine_state after sleeping = %q", b.EngineState)
	}
}
