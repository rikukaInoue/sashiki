package proxy

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// recordRouter は RouteBranch の呼び出しを記録するフェイク Router。
type recordRouter struct {
	mu      sync.Mutex
	routed  []string
	touched []string
	port    int
	err     error
}

func (r *recordRouter) RouteBranch(_ context.Context, name string) (int, error) {
	r.mu.Lock()
	r.routed = append(r.routed, name)
	r.mu.Unlock()
	return r.port, r.err
}

func (r *recordRouter) TouchConn(name string) {
	r.mu.Lock()
	r.touched = append(r.touched, name)
	r.mu.Unlock()
}

func (r *recordRouter) routeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.routed)
}

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.NamePattern == "" {
		cfg.NamePattern = `^[a-z0-9-]{1,32}$`
	}
	s, err := New(cfg, nil) // router は呼び出しごとに差し替える
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// clientHandshake は proxy の初期ハンドシェイクを読み、username と password で
// mysql_native_password 応答を書き、応答パケット(OK/ERR)を1つ読んで返す。
func clientHandshake(t *testing.T, conn net.Conn, username, password string) packet {
	t.Helper()
	hs, err := readPacket(conn)
	if err != nil {
		t.Fatalf("read initial handshake: %v", err)
	}
	salt, _, err := parseBackendHandshake(hs.body)
	if err != nil {
		t.Fatalf("parse handshake: %v", err)
	}
	hr := handshakeResponse{
		caps:     capProtocol41 | capSecureConn | capPluginAuth,
		maxLen:   1 << 24,
		charset:  0xff,
		username: username,
	}
	authResp := nativeToken(password, salt)
	body := buildBackendHandshakeResponse(hr, hr.caps, username, authResp, nativePlugin)
	if err := writePacket(conn, packet{seq: 1, body: body}); err != nil {
		t.Fatalf("write handshake response: %v", err)
	}
	resp, err := readPacket(conn)
	if err != nil {
		t.Fatalf("read auth result: %v", err)
	}
	return resp
}

// runAuth は authTerminate を server 側で走らせ、client 側の 1 パケット応答を返す。
func runAuth(t *testing.T, s *Server, username, password string) packet {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()

	done := make(chan struct{})
	go func() {
		_ = clientConn.SetDeadline(time.Now().Add(3 * time.Second))
		_ = serverConn.SetDeadline(time.Now().Add(3 * time.Second))
		_ = s.authTerminate(context.Background(), serverConn)
		_ = serverConn.Close()
		close(done)
	}()
	resp := clientHandshake(t, clientConn, username, password)
	<-done
	return resp
}

// runAuthSHA2 は caching_sha2_password のクライアントとして authTerminate を
// 駆動する。fast_auth_success(AuthMoreData 0x01 0x03)が来たら消費し、その次の
// 最終パケット(OK / ERR)を返す。fastAuth は 0x03 を観測したか。
func runAuthSHA2(t *testing.T, s *Server, username, password string) (final packet, fastAuth bool) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	done := make(chan struct{})
	go func() {
		_ = clientConn.SetDeadline(time.Now().Add(3 * time.Second))
		_ = serverConn.SetDeadline(time.Now().Add(3 * time.Second))
		_ = s.authTerminate(context.Background(), serverConn)
		_ = serverConn.Close()
		close(done)
	}()
	hs, err := readPacket(clientConn)
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	salt, _, err := parseBackendHandshake(hs.body)
	if err != nil {
		t.Fatalf("parse handshake: %v", err)
	}
	hr := handshakeResponse{caps: capProtocol41 | capSecureConn | capPluginAuth, maxLen: 1 << 24, charset: 0xff, username: username}
	body := buildBackendHandshakeResponse(hr, hr.caps, username, sha2Token(password, salt), sha2Plugin)
	if err := writePacket(clientConn, packet{seq: 1, body: body}); err != nil {
		t.Fatalf("write response: %v", err)
	}
	p, err := readPacket(clientConn)
	if err != nil {
		t.Fatalf("read auth result: %v", err)
	}
	// AuthMoreData(0x01)先頭なら fast_auth_success。消費して次を読む。
	if len(p.body) >= 2 && p.body[0] == 0x01 {
		fastAuth = p.body[1] == 0x03
		if p, err = readPacket(clientConn); err != nil {
			t.Fatalf("read final after AuthMoreData: %v", err)
		}
	}
	<-done
	return p, fastAuth
}

// caching_sha2 クライアントが正しいパスワードなら fast_auth_success を受け取り、
// 認証後に branch へ route される(#197)。
func TestAuthTerminateCachingSha2FastAuth(t *testing.T) {
	r := &recordRouter{err: context.DeadlineExceeded} // route はさせるが未知 branch 扱い
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret"})
	s.router = r
	final, fast := runAuthSHA2(t, s, "dev@pr-1", "s3cret")
	if !fast {
		t.Error("caching_sha2 の正パスワードは fast_auth_success (0x03) を返すはず")
	}
	if !isErr(final.body) {
		t.Error("未知 branch なので最終パケットは ERR のはず")
	}
	if r.routeCount() != 1 || r.routed[0] != "pr-1" {
		t.Errorf("認証成功後にちょうど 1 回 route されるはず (routed=%v)", r.routed)
	}
}

// caching_sha2 の誤パスワードは route せず拒否(fast_auth_success も出さない)。
func TestAuthTerminateCachingSha2WrongPassword(t *testing.T) {
	r := &recordRouter{port: 3401}
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret"})
	s.router = r
	final, fast := runAuthSHA2(t, s, "dev@pr-1", "wrong")
	if fast {
		t.Error("誤パスワードで fast_auth_success を出してはいけない")
	}
	if !isErr(final.body) {
		t.Error("誤パスワードは ERR のはず")
	}
	if r.routeCount() != 0 {
		t.Errorf("認証失敗時は route しないはず (DoS 不変条件), routed=%v", r.routed)
	}
}

func TestAuthTerminateRejectsWrongPasswordWithoutRouting(t *testing.T) {
	r := &recordRouter{port: 3401}
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret"})
	s.router = r

	resp := runAuth(t, s, "dev@pr-1", "wrong-password")
	if !isErr(resp.body) {
		t.Fatalf("wrong password should get an ERR packet, got %v", resp.body)
	}
	// 方式A の要: 認証に失敗したら branch には一切触れない(#7/#51 の DoS 不変条件)。
	if n := r.routeCount(); n != 0 {
		t.Errorf("RouteBranch called %d times on auth failure, want 0 (auth must precede routing)", n)
	}
}

func TestAuthTerminateRejectsMissingBranchWithoutRouting(t *testing.T) {
	r := &recordRouter{port: 3401}
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret"})
	s.router = r

	// user@branch 形式でない(@ が無い)。
	resp := runAuth(t, s, "dev", "s3cret")
	if !isErr(resp.body) {
		t.Fatal("username without @branch should be rejected")
	}
	if n := r.routeCount(); n != 0 {
		t.Errorf("RouteBranch called %d times for malformed username, want 0", n)
	}
}

func TestAuthTerminateEnforcesAllowedUser(t *testing.T) {
	r := &recordRouter{port: 3401}
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret", AllowedUser: "dev"})
	s.router = r

	// パスワードは正しいが user 部が AllowedUser と不一致。
	resp := runAuth(t, s, "mallory@pr-1", "s3cret")
	if !isErr(resp.body) {
		t.Fatal("user not matching AllowedUser should be rejected")
	}
	if n := r.routeCount(); n != 0 {
		t.Errorf("RouteBranch called %d times for disallowed user, want 0", n)
	}
}

func TestAuthTerminateRoutesAfterSuccessfulAuth(t *testing.T) {
	// 認証成功後に branch 解決を試みる。router がエラーを返すと Unknown branch。
	// これで「認証 → route」の順序(認証が先)を確認する。
	r := &recordRouter{err: context.DeadlineExceeded}
	s := newTestServer(t, Config{AppUser: "dev", AppPassword: "s3cret"})
	s.router = r

	resp := runAuth(t, s, "dev@pr-1", "s3cret")
	if !isErr(resp.body) {
		t.Fatal("unknown branch should get an ERR packet")
	}
	if n := r.routeCount(); n != 1 {
		t.Errorf("RouteBranch called %d times, want 1 (routing must happen after successful auth)", n)
	}
	if r.routed[0] != "pr-1" {
		t.Errorf("routed branch = %q, want pr-1", r.routed[0])
	}
}
