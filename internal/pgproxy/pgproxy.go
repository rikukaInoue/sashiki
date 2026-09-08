// Package pgproxy は PostgreSQL 版の固定エンドポイント(既定 :5432)。
// クライアントが startup で送るユーザー名 `<user>@<branch>` から接続先の
// postgres を選ぶ。MySQL の internal/proxy と同じ「認証終端(方式A)」で、
// sashiki が app_user のパスワードを SCRAM-SHA-256 で自身で検証し、
// **検証に成功してから** ブランチを route / lazy create する(認証前に
// リソースを確保させない)。バックエンドへは sashiki が保持する credential で
// 接続し直す。
//
// 既知の制限(v1):
//   - CancelRequest(psql の Ctrl-C)は中継しない。BackendKeyData を素通しして
//     いるため、どのバックエンドへ転送すべきか proxy 側に記録が無い。
//   - クライアント認証は SCRAM-SHA-256 のみ(PostgreSQL 10 以降のクライアントは
//     すべて対応)。
package pgproxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Router はブランチ名から接続先を解決する(workspace.Manager が実装)。
// MySQL 版 proxy.Router と同じ形。
type Router interface {
	RouteBranch(ctx context.Context, name string) (port int, err error)
	TouchConn(name string)
}

// Config はプロキシの設定。
type Config struct {
	Listen           string // "0.0.0.0:5432"
	NamePattern      string
	BackendHost      string // 既定 127.0.0.1
	MaxConnPerBranch int    // 既定 50
	// AllowedUser が非空なら、<user>@<branch> の user 部がこれと一致する接続
	// だけを受け付ける(認証前の第一関門)。
	AllowedUser string
	// AppUser / AppPassword は方式A の app credential。クライアント認証の検証と
	// バックエンド接続の両方に使う。
	AppUser     string
	AppPassword string
	// TLSConfig が非 nil なら SSLRequest に応じて TLS 終端する。
	TLSConfig *tls.Config
}

// Server は PostgreSQL プロトコルプロキシ。
type Server struct {
	cfg    Config
	router Router
	nameRe *regexp.Regexp

	mu    sync.Mutex
	conns map[string]int
}

// New は Server を作る。
func New(cfg Config, router Router) (*Server, error) {
	if cfg.BackendHost == "" {
		cfg.BackendHost = "127.0.0.1"
	}
	if cfg.MaxConnPerBranch == 0 {
		cfg.MaxConnPerBranch = 50
	}
	if cfg.AppUser == "" {
		cfg.AppUser = "dev"
	}
	re, err := regexp.Compile(cfg.NamePattern)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, router: router, nameRe: re, conns: map[string]int{}}, nil
}

// Listen は接続を受け付ける。ctx キャンセルで停止。
func (s *Server) Listen(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("pgproxy listen: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) acquire(branch string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[branch] >= s.cfg.MaxConnPerBranch {
		return false
	}
	s.conns[branch]++
	return true
}

// ActiveConns はブランチの現在の素通し接続数を返す(リーパーの使用中判定用)。
func (s *Server) ActiveConns(branch string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[branch]
}

func (s *Server) release(branch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[branch] > 0 {
		s.conns[branch]--
	}
}

func (s *Server) handle(ctx context.Context, client net.Conn) {
	defer func() { _ = client.Close() }()
	enableKeepAlive(client)
	_ = client.SetDeadline(time.Now().Add(60 * time.Second))
	if err := s.authTerminate(ctx, client); err != nil {
		log.Printf("pgproxy: %v", err)
	}
}

// fatal は ErrorResponse を送って接続を切る。
func fatal(c net.Conn, sqlstate, msg string) error {
	_ = writeMessage(c, msgErrorResponse, buildError(sqlstate, msg))
	return fmt.Errorf("rejected: %s", msg)
}

// authTerminate は認証フェーズ。クライアントを SCRAM で検証してから
// branch を route / lazy create し、バックエンドへ繋ぎ直して素通しに入る。
func (s *Server) authTerminate(ctx context.Context, client net.Conn) error {
	su, err := s.readStartupWithTLS(&client)
	if err != nil {
		return err
	}

	switch su.code {
	case codeCancelRequest:
		// v1 では中継しない。クライアントは応答を期待しないので黙って閉じる。
		log.Printf("pgproxy: CancelRequest は未対応のため無視します")
		return nil
	case protocolV3:
	default:
		return fmt.Errorf("unsupported startup code %d", su.code)
	}

	rawUser := su.params["user"]
	user, branch, ok := strings.Cut(rawUser, "@")
	if !ok || !s.nameRe.MatchString(branch) {
		return fatal(client, "28000",
			fmt.Sprintf("user must be <user>@<branch> (got %q)", rawUser))
	}
	if s.cfg.AllowedUser != "" && user != s.cfg.AllowedUser {
		return fatal(client, "28000", fmt.Sprintf("access denied for user %q", user))
	}

	// 認証終端: app パスワードで検証する。ここを通るまで branch に触れない。
	if err := s.verifyClientSCRAM(client); err != nil {
		return fatal(client, "28P01", fmt.Sprintf("password authentication failed for user %q", rawUser))
	}

	// 認証済み → branch 解決(必要なら lazy create)
	port, err := s.router.RouteBranch(ctx, branch)
	if err != nil {
		return fatal(client, "3D000", fmt.Sprintf("unknown branch %q", branch))
	}
	if !s.acquire(branch) {
		return fatal(client, "53300", "too many connections for branch")
	}
	defer s.release(branch)

	// バックエンドへ sashiki 保持の credential で接続し直す
	backend, err := net.DialTimeout("tcp",
		fmt.Sprintf("%s:%d", s.cfg.BackendHost, port), 10*time.Second)
	if err != nil {
		return fatal(client, "08006", "backend unavailable")
	}
	defer func() { _ = backend.Close() }()
	if err := authenticateBackend(backend, s.cfg.AppUser, su.params["database"], s.cfg.AppPassword); err != nil {
		log.Printf("pgproxy: backend auth for %s@%s: %v", user, branch, err)
		return fatal(client, "08006", "backend authentication failed")
	}

	// クライアントへ認証完了を返す。以降 ParameterStatus / BackendKeyData /
	// ReadyForQuery はバックエンドのものがそのまま素通しで届く。
	if err := writeMessage(client, msgAuthentication, buildAuthOK()); err != nil {
		return err
	}

	s.router.TouchConn(branch)
	enableKeepAlive(backend)
	_ = client.SetDeadline(time.Time{})
	pipe(client, backend)
	return nil
}

// readStartupWithTLS は SSLRequest / GSSENCRequest を捌いてから本来の
// StartupMessage を返す。TLS 未設定なら 'N' を返して平文で続行する。
func (s *Server) readStartupWithTLS(client *net.Conn) (startup, error) {
	for {
		su, err := readStartup(*client)
		if err != nil {
			return startup{}, err
		}
		switch su.code {
		case codeSSLRequest:
			if s.cfg.TLSConfig == nil {
				if _, err := (*client).Write([]byte{'N'}); err != nil {
					return startup{}, err
				}
				continue // 平文のまま StartupMessage を読み直す
			}
			if _, err := (*client).Write([]byte{'S'}); err != nil {
				return startup{}, err
			}
			tlsConn := tls.Server(*client, s.cfg.TLSConfig)
			if err := tlsConn.Handshake(); err != nil {
				return startup{}, fmt.Errorf("tls handshake: %w", err)
			}
			*client = tlsConn
			continue
		case codeGSSENCRequest:
			// GSSAPI 暗号化は非対応。'N' を返せばクライアントは平文か SSL に落ちる。
			if _, err := (*client).Write([]byte{'N'}); err != nil {
				return startup{}, err
			}
			continue
		default:
			return su, nil
		}
	}
}

// verifyClientSCRAM は SCRAM-SHA-256 でクライアントのパスワードを検証する。
func (s *Server) verifyClientSCRAM(client net.Conn) error {
	// SCRAM-SHA-256 のみ広告する(メカニズム名の列を null で終端し、さらに null)。
	mechs := append([]byte("SCRAM-SHA-256"), 0, 0)
	if err := writeMessage(client, msgAuthentication, buildAuthMessage(authSASL, mechs)); err != nil {
		return err
	}

	m, err := readMessage(client)
	if err != nil {
		return err
	}
	if m.typ != msgPassword {
		return fmt.Errorf("expected SASLInitialResponse, got %q", m.typ)
	}
	mech, rest, err := cstring(m.body)
	if err != nil {
		return err
	}
	if mech != "SCRAM-SHA-256" {
		return fmt.Errorf("unsupported SASL mechanism %q", mech)
	}
	if len(rest) < 4 {
		return fmt.Errorf("malformed SASLInitialResponse")
	}
	initial := rest[4:] // 先頭 4 byte はデータ長

	sv := &scramServer{password: s.cfg.AppPassword}
	serverFirst, err := sv.firstReply(initial)
	if err != nil {
		return err
	}
	if err := writeMessage(client, msgAuthentication,
		buildAuthMessage(authSASLContinue, []byte(serverFirst))); err != nil {
		return err
	}

	m, err = readMessage(client)
	if err != nil {
		return err
	}
	if m.typ != msgPassword {
		return fmt.Errorf("expected SASLResponse, got %q", m.typ)
	}
	serverFinal, err := sv.finalReply(m.body)
	if err != nil {
		return err
	}
	return writeMessage(client, msgAuthentication,
		buildAuthMessage(authSASLFinal, []byte(serverFinal)))
}

// closeWriter は書き込み側だけを閉じられる接続。
type closeWriter interface{ CloseWrite() error }

// pipe は認証後を双方向に素通しする。片方向の EOF で全体を切らず、その向きだけ
// half-close してもう片方向を完走させる(MySQL 版と同じ理由: バックエンドが
// idle_stop_after で消えたときにクライアントへ綺麗な EOF を伝える)。
func pipe(client, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(backend, client)
		if cw, ok := backend.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, backend)
		if cw, ok := client.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
}

// enableKeepAlive は TCP keepalive を有効化して、消えた peer を早く検知する。
func enableKeepAlive(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.SetKeepAlive(true)
		_ = t.SetKeepAlivePeriod(30 * time.Second)
	}
}
