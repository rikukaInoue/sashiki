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
	"strings"
	"time"

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
	s.mux.HandleFunc("GET /v1/branches/{name}/schema", s.handleSchema)
	s.mux.HandleFunc("POST /v1/branches/{name}/query", s.handleQuery)
	s.mux.HandleFunc("DELETE /v1/branches/{name}", s.handleDelete)
	s.mux.HandleFunc("GET /v1/baseline", s.handleBaseline)
	s.mux.HandleFunc("POST /v1/baseline/refresh", s.handleBaselineRefresh)
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
	Name string `json:"name"`
	Port int    `json:"port,omitempty"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_name", "invalid request body")
		return
	}
	existOK := r.URL.Query().Get("exist_ok") == "true"
	var info workspace.Info
	opID, err := s.track("create", req.Name, func() error {
		var e error
		info, e = s.mgr.Create(r.Context(), req.Name, req.Port)
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
	HookStatus     map[string]string `json:"hook_status,omitempty"`
	Error          string            `json:"error,omitempty"`
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
		HookStatus:     i.HookStatus,
		Error:          i.ErrorMessage,
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
