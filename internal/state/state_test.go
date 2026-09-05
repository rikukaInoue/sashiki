package state

import (
	"errors"
	"path/filepath"
	"testing"
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
