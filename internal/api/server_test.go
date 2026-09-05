package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rikukaInoue/twig/internal/branch"
	"github.com/rikukaInoue/twig/internal/engine"
	"github.com/rikukaInoue/twig/internal/state"
	"github.com/rikukaInoue/twig/internal/storage"
)

type fakeStorage struct{}

func (fakeStorage) Capabilities() storage.Capabilities {
	return storage.Capabilities{FastRollback: true}
}
func (fakeStorage) Clone(ctx context.Context, b storage.SnapshotRef, name string) (storage.Volume, error) {
	return storage.Volume{Name: name, Dataset: "p/b/" + name, Path: "/p/b/" + name}, nil
}
func (fakeStorage) SnapshotInit(ctx context.Context, v storage.Volume) (storage.SnapshotRef, error) {
	return storage.SnapshotRef(v.Dataset + "@init"), nil
}
func (fakeStorage) Rollback(ctx context.Context, v storage.Volume, s storage.SnapshotRef) error {
	return nil
}
func (fakeStorage) DeleteAsync(ctx context.Context, v storage.Volume) (storage.JobID, error) {
	return "done", nil
}
func (fakeStorage) Poll(ctx context.Context, j storage.JobID) (storage.JobStatus, error) {
	return storage.JobCompleted, nil
}
func (fakeStorage) SnapshotBase(ctx context.Context, tag string) (storage.SnapshotRef, error) {
	return "", nil
}
func (fakeStorage) ListSnapshots(ctx context.Context) ([]storage.SnapshotRef, error) {
	return []storage.SnapshotRef{"p/base@baseline", "p/base@baseline-20260901"}, nil
}
func (fakeStorage) UsedBytes(ctx context.Context, v storage.Volume) (int64, error) { return 42, nil }
func (fakeStorage) CurrentBaseline() storage.SnapshotRef                           { return "p/base@baseline" }

type fakeEngine struct{}

func (fakeEngine) Start(ctx context.Context, i engine.Instance) error     { return nil }
func (fakeEngine) Stop(ctx context.Context, i engine.Instance) error      { return nil }
func (fakeEngine) WaitReady(ctx context.Context, i engine.Instance) error { return nil }
func (fakeEngine) IsRunning(ctx context.Context, i engine.Instance) (bool, error) {
	return true, nil
}

func newTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fs := fakeStorage{}
	mgr, err := branch.New(branch.Config{
		NamePattern: `^[a-z0-9-]{1,32}$`, MaxBranches: 10,
		PortLow: 3401, PortHigh: 3410, EngineType: "mysql", StateDir: t.TempDir(),
	}, fs, fs, fakeEngine{}, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(mgr, "twig.internal", "dev", "dev", token, nil))
	t.Cleanup(srv.Close)
	return srv
}

func TestAPILifecycle(t *testing.T) {
	srv := newTestServer(t, "")

	// create
	resp, err := http.Post(srv.URL+"/v1/branches", "application/json",
		strings.NewReader(`{"name":"pr-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var b struct {
		Name  string `json:"name"`
		State string `json:"state"`
		User  string `json:"user"`
		Host  string `json:"host"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&b)
	_ = resp.Body.Close()
	if b.State != "running" || b.User != "dev@pr-1" || b.Host != "twig.internal" {
		t.Errorf("branch = %+v", b)
	}

	// duplicate → 409
	resp, _ = http.Post(srv.URL+"/v1/branches", "application/json", strings.NewReader(`{"name":"pr-1"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("dup status = %d, want 409", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// exist_ok=true → 200
	resp, _ = http.Post(srv.URL+"/v1/branches?exist_ok=true", "application/json", strings.NewReader(`{"name":"pr-1"}`))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("exist_ok status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// invalid name → 400
	resp, _ = http.Post(srv.URL+"/v1/branches", "application/json", strings.NewReader(`{"name":"BAD NAME"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// reset
	resp, _ = http.Post(srv.URL+"/v1/branches/pr-1/reset", "application/json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("reset status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// delete
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/branches/pr-1", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// not found → 404
	resp, _ = http.Get(srv.URL + "/v1/branches/pr-1")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestAPIAuthFromNonLoopback(t *testing.T) {
	// httptest は loopback なので、authorized() を直接検証する。
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := branch.New(branch.Config{NamePattern: `^.+$`, PortLow: 1, PortHigh: 2, EngineType: "mysql"}, fs, fs, fakeEngine{}, nil, db)
	s := New(mgr, "d", "dev", "dev", "secret", db)

	req := httptest.NewRequest(http.MethodGet, "/v1/branches", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	if s.authorized(req) {
		t.Error("no token from non-loopback should be denied")
	}
	req.Header.Set("Authorization", "Bearer wrong")
	if s.authorized(req) {
		t.Error("wrong token should be denied")
	}
	req.Header.Set("Authorization", "Bearer secret")
	if !s.authorized(req) {
		t.Error("correct token should be allowed")
	}
	// loopback は無認証
	req2 := httptest.NewRequest(http.MethodGet, "/v1/branches", nil)
	req2.RemoteAddr = "127.0.0.1:9999"
	if !s.authorized(req2) {
		t.Error("loopback should be allowed without token")
	}
}

func TestAPIBaseline(t *testing.T) {
	srv := newTestServer(t, "")
	resp, err := http.Get(srv.URL + "/v1/baseline")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var b struct {
		Current   string   `json:"current"`
		Snapshots []string `json:"snapshots"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&b)
	if b.Current != "p/base@baseline" {
		t.Errorf("current = %q", b.Current)
	}
	if len(b.Snapshots) != 2 {
		t.Errorf("snapshots = %v", b.Snapshots)
	}
}

func TestAPIAuthWithDBToken(t *testing.T) {
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := branch.New(branch.Config{NamePattern: `^.+$`, PortLow: 1, PortHigh: 2, EngineType: "mysql"}, fs, fs, fakeEngine{}, nil, db)
	s := New(mgr, "d", "dev", "dev", "", db)

	// トークン登録(平文 "twig_abc" のハッシュ)
	sum := sha256.Sum256([]byte("twig_abc"))
	if err := db.CreateToken("t1", hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/branches", nil)
	req.RemoteAddr = "10.0.0.5:1"
	req.Header.Set("Authorization", "Bearer twig_abc")
	if !s.authorized(req) {
		t.Error("db token should be accepted")
	}
	req.Header.Set("Authorization", "Bearer twig_wrong")
	if s.authorized(req) {
		t.Error("wrong token should be denied")
	}
	// revoke 後は拒否
	_ = db.RevokeToken("t1")
	req.Header.Set("Authorization", "Bearer twig_abc")
	if s.authorized(req) {
		t.Error("revoked token should be denied")
	}
}

func TestMetricsAndWebUI(t *testing.T) {
	srv := newTestServer(t, "")
	// ブランチを1つ作ってから
	resp, _ := http.Post(srv.URL+"/v1/branches", "application/json", strings.NewReader(`{"name":"pr-m"}`))
	_ = resp.Body.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("webui status=%d type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestMetricsHandler(t *testing.T) {
	db, _ := state.Open(filepath.Join(t.TempDir(), "s.db"))
	defer func() { _ = db.Close() }()
	fs := fakeStorage{}
	mgr, _ := branch.New(branch.Config{NamePattern: `^.+$`, PortLow: 3401, PortHigh: 3410, EngineType: "mysql", StateDir: t.TempDir()}, fs, fs, fakeEngine{}, nil, db)
	_, _ = mgr.Create(context.Background(), "pr-1", 0)

	ms := httptest.NewServer(MetricsHandler(mgr))
	defer ms.Close()
	resp, err := http.Get(ms.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := new(strings.Builder)
	_, _ = io.Copy(body, resp.Body)
	out := body.String()
	if !strings.Contains(out, `twig_branches{state="running"} 1`) {
		t.Errorf("metrics missing running gauge:\n%s", out)
	}
	if !strings.Contains(out, `twig_branch_used_bytes{branch="pr-1"} 42`) {
		t.Errorf("metrics missing used bytes:\n%s", out)
	}
}
