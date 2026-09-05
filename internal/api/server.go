// Package api は REST API(仕様 14-1)。プロキシ(v0.2)は含まない。
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rikukaInoue/twig/internal/branch"
	"github.com/rikukaInoue/twig/internal/state"
)

// Server は REST API サーバー。
type Server struct {
	mgr    *branch.Manager
	domain string
	user   string
	token  string // 空なら外部トークン認証なし(localhost のみ想定)
	mux    *http.ServeMux
}

// New は Server を作る。
func New(mgr *branch.Manager, domain, proxyUser, token string) *Server {
	s := &Server{mgr: mgr, domain: domain, user: proxyUser, token: token, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /v1/branches", s.handleList)
	s.mux.HandleFunc("POST /v1/branches", s.handleCreate)
	s.mux.HandleFunc("GET /v1/branches/{name}", s.handleGet)
	s.mux.HandleFunc("POST /v1/branches/{name}/reset", s.handleReset)
	s.mux.HandleFunc("DELETE /v1/branches/{name}", s.handleDelete)
	s.mux.HandleFunc("GET /v1/baseline", s.handleBaseline)
	s.mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
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
	if s.token == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	gh := sha256.Sum256([]byte(got))
	th := sha256.Sum256([]byte(s.token))
	return subtle.ConstantTimeCompare(gh[:], th[:]) == 1
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
		"current":   info.Current,
		"snapshots": info.Snapshots,
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
	info, err := s.mgr.Create(r.Context(), req.Name, req.Port)
	if errors.Is(err, branch.ErrExists) && existOK {
		if info, err = s.mgr.Get(r.Context(), req.Name); err == nil {
			writeJSON(w, http.StatusOK, s.toJSON(info))
			return
		}
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
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
	info, err := s.mgr.Reset(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toJSON(info))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Delete(r.Context(), r.PathValue("name")); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

func (s *Server) toJSON(i branch.Info) branchJSON {
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
	case errors.Is(err, branch.ErrInvalidName):
		writeErr(w, http.StatusBadRequest, "invalid_name", err.Error())
	case errors.Is(err, branch.ErrExists):
		writeErr(w, http.StatusConflict, "branch_exists", err.Error())
	case errors.Is(err, state.ErrNotFound):
		writeErr(w, http.StatusNotFound, "branch_not_found", err.Error())
	case errors.Is(err, branch.ErrLimitReached), errors.Is(err, branch.ErrNoFreePort):
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
