// Package api は REST API(仕様 14-1)。プロキシ(v0.2)は含まない。
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rikukaInoue/sashiki/internal/hooks"
	"github.com/rikukaInoue/sashiki/internal/ops"
	"github.com/rikukaInoue/sashiki/internal/state"
	"github.com/rikukaInoue/sashiki/internal/workspace"
)

// TokenChecker は Bearer トークンの検証(state.db の tokens テーブル)。
type TokenChecker interface {
	CheckTokenHash(hash string) (bool, error)
}

// Server は REST API サーバー。
type Server struct {
	mgr    *workspace.Manager
	domain string
	engine string // mysql | postgres。データブラウザは mysql のみ対応
	user   string
	pass   string
	token  string // 環境変数トークン(後方互換)。空なら無効
	tokens TokenChecker
	openDB func(ctx context.Context, name string) (*sql.DB, error) // テストで差し替え可
	ops    *ops.Runner                                             // nil 可(operation 記録なし)
	mux    *http.ServeMux
}

// SetOps は operation Runner を配線する(sashikid 起動時)。
func (s *Server) SetOps(r *ops.Runner) {
	s.ops = r
	s.mux.HandleFunc("GET /v1/operations", s.handleListOps)
	s.mux.HandleFunc("GET /v1/operations/{id}", s.handleGetOp)
}

// track は typ/target の operation を記録しつつ fn を同期実行する。
// operation_id を返す(ops 未配線なら空)。
func (s *Server) track(typ, target string, fn func() error) (string, error) {
	if s.ops == nil {
		return "", fn()
	}
	return s.ops.RunSync(typ, target, func(context.Context) error { return fn() })
}

// New は Server を作る。tokens は nil 可(env トークンのみ)。
func New(mgr *workspace.Manager, domain, engineType, proxyUser, proxyPass, token string, tokens TokenChecker) *Server {
	s := &Server{mgr: mgr, domain: domain, engine: engineType, user: proxyUser, pass: proxyPass, token: token, tokens: tokens, mux: http.NewServeMux()}
	s.openDB = s.branchDB
	s.mux.HandleFunc("GET /v1/branches", s.handleList)
	s.mux.HandleFunc("POST /v1/branches", s.handleCreate)
	s.mux.HandleFunc("GET /v1/branches/{name}", s.handleGet)
	s.mux.HandleFunc("POST /v1/branches/{name}/reset", s.handleReset)
	s.mux.HandleFunc("POST /v1/branches/{name}/recreate", s.handleRecreate)
	s.mux.HandleFunc("POST /v1/branches/{name}/wake", s.handleWake)
	s.mux.HandleFunc("POST /v1/branches/{name}/retry", s.handleRetry)
	s.mux.HandleFunc("POST /v1/branches/{name}/lease", s.handleLease)
	s.mux.HandleFunc("POST /v1/branches/{name}/hooks/{event}", s.handleRunHook)
	s.mux.HandleFunc("GET /v1/branches/{name}/schema", s.handleSchema)
	s.mux.HandleFunc("POST /v1/branches/{name}/query", s.handleQuery)
	s.mux.HandleFunc("DELETE /v1/branches/{name}", s.handleDelete)
	s.mux.HandleFunc("GET /v1/baseline", s.handleBaseline)
	s.mux.HandleFunc("POST /v1/baseline/refresh", s.handleBaselineRefresh)
	s.mux.HandleFunc("GET /v1/baselines", s.handleListBaselines)
	s.mux.HandleFunc("POST /v1/baseline/set", s.handleSetBaseline)
	s.mux.HandleFunc("POST /v1/baseline/gc", s.handleGCBaselines)
	s.mux.HandleFunc("GET /v1/capacity", s.handleCapacity)
	s.mux.HandleFunc("GET /v1/doctor", s.handleDoctor)
	s.mux.HandleFunc("POST /v1/gc/orphans", s.handleGCOrphans)
	s.mux.HandleFunc("POST /v1/drain", s.handleDrain)
	s.mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /", s.handleWebUI)
	return s
}

// ServeHTTP は認証を通してからルーティングする。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid token")
		return
	}
	s.mux.ServeHTTP(w, r)
}

// authorized: localhost からは無認証、それ以外は Bearer トークン(仕様 13-3)。
func (s *Server) authorized(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return true
		}
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		return false
	}
	gh := sha256.Sum256([]byte(got))
	if s.token != "" {
		th := sha256.Sum256([]byte(s.token))
		if subtle.ConstantTimeCompare(gh[:], th[:]) == 1 {
			return true
		}
	}
	if s.tokens != nil {
		ok, err := s.tokens.CheckTokenHash(hex.EncodeToString(gh[:]))
		if err != nil {
			// DB 障害を無言の 401 にしない(認証失敗とは区別してログに残す)
			log.Printf("api: token check failed: %v", err)
			return false
		}
		if ok {
			return true
		}
	}
	return false
}

// --- handlers ---

func (s *Server) handleBaseline(w http.ResponseWriter, r *http.Request) {
	info, err := s.mgr.Baseline(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	if info.Snapshots == nil {
		info.Snapshots = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current":            info.Current,
		"snapshots":          info.Snapshots,
		"refreshing":         workspace.RefreshInProgress(),
		"last_refresh_error": workspace.RefreshLastError(),
	})
}

func (s *Server) handleBaselineRefresh(w http.ResponseWriter, r *http.Request) {
	tag, err := s.mgr.RefreshBaseline(r.Context(), workspace.RefreshConfig{})
	if errors.Is(err, workspace.ErrRefreshRunning) {
		writeErr(w, http.StatusConflict, "refresh_running", err.Error())
		return
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "tag": tag})
}

func (s *Server) handleListBaselines(w http.ResponseWriter, r *http.Request) {
	rows, err := s.mgr.ListBaselineRows()
	if err != nil {
		s.writeError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, b := range rows {
		out = append(out, map[string]any{
			"snapshot":        b.Snapshot,
			"created_at":      b.CreatedAt.UTC().Format(time.RFC3339),
			"is_current":      b.IsCurrent,
			"schema_revision": b.Prov.SchemaRevision,
			"data_as_of":      b.Prov.DataAsOf,
			"masked":          b.Prov.Masked,
			"validated":       b.Prov.Validated,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"baselines": out})
}

func (s *Server) handleSetBaseline(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Snapshot == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "body must be {\"snapshot\": \"...\"}")
		return
	}
	if err := s.mgr.SetBaseline(r.Context(), req.Snapshot); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"current": req.Snapshot})
}

func (s *Server) handleGCBaselines(w http.ResponseWriter, r *http.Request) {
	keepLast := 3
	if v := r.URL.Query().Get("keep_last"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &keepLast)
	}
	res, err := s.mgr.GCBaselines(r.Context(), workspace.GCConfig{KeepLast: keepLast})
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": res.Deleted, "kept": res.Kept})
}

func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	c, err := s.mgr.Capacity(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"storage": map[string]any{
			"pool_used_bytes":    c.PoolUsedBytes,
			"pool_total_bytes":   c.PoolTotalBytes,
			"pool_used_ratio":    c.PoolUsedRatio,
			"high_watermark":     c.HighWatermark,
			"critical_watermark": c.CritWatermark,
		},
		"ports":    map[string]any{"used": c.PortsUsed, "total": c.PortsTotal},
		"memory":   map[string]any{"available_bytes": c.MemAvailableBytes, "expected_rss_bytes": c.ExpectedRSSBytes},
		"branches": map[string]any{"running": c.Running, "max_running": c.MaxRunning, "max_branches": c.MaxBranches},
	})
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	d, err := s.mgr.Doctor(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pool_healthy":       d.PoolHealthy,
		"pool_used_ratio":    d.PoolUsedRatio,
		"current_baseline":   d.CurrentBaseline,
		"branch_count":       d.BranchCount,
		"port_conflicts":     d.PortConflicts,
		"orphans":            d.Orphans,
		"memory_headroom_ok": d.MemHeadroomOK,
		"issues":             d.Issues,
	})
}

func (s *Server) handleGCOrphans(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.mgr.GCOrphans(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

// handleDrain は全 running branch を sleeping にする(POST /v1/drain, 仕様17章)。
// instance_class 変更前などに mysqld を安全に落とすのに使う。
func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	res, err := s.mgr.Drain(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"slept": res.Slept, "skipped": res.Skipped, "failed": res.Failed,
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	infos, err := s.mgr.List(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	out := make([]branchJSON, 0, len(infos))
	for _, i := range infos {
		out = append(out, s.toJSON(i))
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": out})
}

type createReq struct {
	Name    string          `json:"name"`
	Port    int             `json:"port,omitempty"`
	Profile string          `json:"profile,omitempty"`
	Owner   string          `json:"owner,omitempty"`
	Purpose string          `json:"purpose,omitempty"`
	Source  json.RawMessage `json:"source,omitempty"`
	TTL     string          `json:"ttl,omitempty"` // 初期 lease 期限(例 "7d","1h"）。空なら無期限
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_name", "invalid request body")
		return
	}
	existOK := r.URL.Query().Get("exist_ok") == "true"
	var ttl time.Duration
	if req.TTL != "" {
		var perr error
		if ttl, perr = parseDur(req.TTL); perr != nil {
			writeErr(w, http.StatusBadRequest, "invalid_name", "invalid ttl: "+perr.Error())
			return
		}
	}
	var info workspace.Info
	meta := state.Meta{Profile: req.Profile, Owner: req.Owner, Purpose: req.Purpose, Source: string(req.Source)}
	opID, err := s.track("create", req.Name, func() error {
		var e error
		info, e = s.mgr.CreateWithMeta(r.Context(), req.Name, req.Port, meta)
		if e != nil {
			return e
		}
		if ttl > 0 {
			if _, e = s.mgr.Lease(r.Context(), req.Name, ttl); e != nil {
				return e
			}
			info, e = s.mgr.Get(r.Context(), req.Name) // expires_at を反映
		}
		return e
	})
	if errors.Is(err, workspace.ErrExists) && existOK {
		if info, err = s.mgr.Get(r.Context(), req.Name); err == nil {
			writeJSON(w, http.StatusOK, s.toJSON(info))
			return
		}
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	writeJSON(w, http.StatusCreated, s.toJSON(info))
}

type leaseReq struct {
	For string `json:"for"` // 追加する期間(例 "7d")。now からの新しい expires_at を設定
}

// handleLease は lease を renew する(POST /v1/branches/{name}/lease)。
// expires_at を now+for に(再)設定する。冪等ではなく「今から for 後まで延長」。
func (s *Server) handleLease(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req leaseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.For == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "invalid request body (need {\"for\":\"7d\"})")
		return
	}
	d, perr := parseDur(req.For)
	if perr != nil {
		writeErr(w, http.StatusBadRequest, "invalid_name", "invalid for: "+perr.Error())
		return
	}
	var info workspace.Info
	opID, err := s.track("lease", name, func() error {
		if _, e := s.mgr.Lease(r.Context(), name, d); e != nil {
			return e
		}
		var e error
		info, e = s.mgr.Get(r.Context(), name)
		return e
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

// parseDur は time.ParseDuration に加えて末尾 d(日)/w(週)を許す。
func parseDur(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n := len(s); n >= 2 {
		switch s[n-1] {
		case 'd', 'w':
			num, err := strconv.ParseFloat(s[:n-1], 64)
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			base := 24 * time.Hour
			if s[n-1] == 'w' {
				base = 7 * 24 * time.Hour
			}
			return time.Duration(num * float64(base)), nil
		}
	}
	return time.ParseDuration(s)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	info, err := s.mgr.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var info workspace.Info
	opID, err := s.track("reset", name, func() error {
		var e error
		info, e = s.mgr.Reset(r.Context(), name)
		return e
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

func (s *Server) handleRecreate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var info workspace.Info
	opID, err := s.track("recreate", name, func() error {
		var e error
		info, e = s.mgr.Recreate(r.Context(), name)
		return e
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var info workspace.Info
	opID, err := s.track("wake", name, func() error {
		var e error
		info, e = s.mgr.Wake(r.Context(), name)
		return e
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var info workspace.Info
	opID, err := s.track("retry", name, func() error {
		var e error
		info, e = s.mgr.Retry(r.Context(), name)
		return e
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

func (s *Server) handleRunHook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	event := hooks.Event(r.PathValue("event"))
	if err := s.mgr.RunHookManually(r.Context(), name, event); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ran", "event": string(event)})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	opID, err := s.track("delete", name, func() error {
		return s.mgr.Delete(r.Context(), name)
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	setOpID(w, opID)
	w.WriteHeader(http.StatusNoContent)
}

func setOpID(w http.ResponseWriter, id string) {
	if id != "" {
		w.Header().Set("Sashiki-Operation-Id", id)
	}
}

func (s *Server) handleListOps(w http.ResponseWriter, r *http.Request) {
	list, err := s.ops.List(50)
	if err != nil {
		s.writeError(w, err)
		return
	}
	out := make([]opJSON, 0, len(list))
	for _, o := range list {
		out = append(out, toOpJSON(o))
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": out})
}

func (s *Server) handleGetOp(w http.ResponseWriter, r *http.Request) {
	op, err := s.ops.Get(r.PathValue("id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toOpJSON(op))
}

type opJSON struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Target     string  `json:"target"`
	State      string  `json:"state"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at,omitempty"`
	Error      string  `json:"error,omitempty"`
}

func toOpJSON(o state.Operation) opJSON {
	j := opJSON{
		ID: o.ID, Type: o.Type, Target: o.Target, State: o.State,
		StartedAt: o.StartedAt.UTC().Format(time.RFC3339), Error: o.Error,
	}
	if o.FinishedAt != nil {
		f := o.FinishedAt.UTC().Format(time.RFC3339)
		j.FinishedAt = &f
	}
	return j
}

// --- serialization ---

type branchJSON struct {
	Name           string            `json:"name"`
	State          string            `json:"state"`
	Port           int               `json:"port"`
	Host           string            `json:"host"`
	User           string            `json:"user"`
	OriginSnapshot string            `json:"origin_snapshot"`
	CreatedAt      string            `json:"created_at"`
	LastConnAt     *string           `json:"last_conn_at,omitempty"`
	UsedBytes      int64             `json:"used_bytes"`
	LogicalBytes   int64             `json:"logical_bytes,omitempty"`
	HookStatus     map[string]string `json:"hook_status,omitempty"`
	Error          string            `json:"error,omitempty"`
	FailedOp       string            `json:"failed_operation,omitempty"`
	ErrorCode      string            `json:"error_code,omitempty"`
	Recoverable    bool              `json:"recoverable,omitempty"`
	Suggestions    []string          `json:"suggested_actions,omitempty"`
	Profile        string            `json:"profile,omitempty"`
	Owner          string            `json:"owner,omitempty"`
	Purpose        string            `json:"purpose,omitempty"`
	Source         json.RawMessage   `json:"source,omitempty"`
	ExpiresAt      *string           `json:"expires_at,omitempty"`
}

func (s *Server) toJSON(i workspace.Info) branchJSON {
	b := branchJSON{
		Name:           i.Name,
		State:          i.State,
		Port:           i.Port,
		Host:           s.domain,
		User:           s.user + "@" + i.Name,
		OriginSnapshot: i.OriginSnapshot,
		CreatedAt:      i.CreatedAt.UTC().Format(time.RFC3339),
		UsedBytes:      i.UsedBytes,
		LogicalBytes:   i.LogicalBytes,
		HookStatus:     i.HookStatus,
		Error:          i.ErrorMessage,
		FailedOp:       i.FailedOp,
		ErrorCode:      i.ErrorCode,
		Recoverable:    i.Recoverable,
		Suggestions:    i.SuggestedActions,
		Profile:        i.Profile,
		Owner:          i.Owner,
		Purpose:        i.Purpose,
	}
	if i.Source != "" {
		b.Source = json.RawMessage(i.Source)
	}
	if i.ExpiresAt != nil {
		e := i.ExpiresAt.UTC().Format(time.RFC3339)
		b.ExpiresAt = &e
	}
	if i.LastConnAt != nil {
		t := i.LastConnAt.UTC().Format(time.RFC3339)
		b.LastConnAt = &t
	}
	return b
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnsupportedEngine):
		writeErr(w, http.StatusNotImplemented, "unsupported_engine", err.Error())
	case errors.Is(err, workspace.ErrInvalidName):
		writeErr(w, http.StatusBadRequest, "invalid_name", err.Error())
	case errors.Is(err, workspace.ErrExists):
		writeErr(w, http.StatusConflict, "branch_exists", err.Error())
	case errors.Is(err, state.ErrNotFound):
		writeErr(w, http.StatusNotFound, "branch_not_found", err.Error())
	case errors.Is(err, workspace.ErrLimitReached), errors.Is(err, workspace.ErrNoFreePort):
		writeErr(w, http.StatusInsufficientStorage, "limit_reached", err.Error())
	case strings.Contains(err.Error(), "hook"):
		writeErr(w, http.StatusInternalServerError, "hook_failed", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "storage_error", err.Error())
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, ecode, msg string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]string{"code": ecode, "message": msg},
	})
}

// Listen はサーバーを起動する。
func (s *Server) Listen(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errCh:
		return fmt.Errorf("api server: %w", err)
	}
}
