// Package proxy は :3306 の固定エンドポイント。クライアントのユーザー名
// `<user>@<branch>` からバックエンドの mysqld を選ぶ。
//
// 方式A(認証終端, #51): sashiki が app_user のパスワードを保持し、クライアントの
// mysql_native_password 認証を自身で検証する。**検証に成功してから**ブランチを
// route / lazy create するため、認証前の無償リソース確保(#7 の DoS 構造)が
// 起きない。バックエンドへは sashiki が保持する credential で接続し直す。
// TLS 終端は cert 指定時のみ有効(クライアント↔sashiki=TLS、sashiki↔backend=
// localhost 平文)。以前の認証中継(ADR-006)は DECISIONS.md に経緯として残す。
package proxy

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
	"sync/atomic"
	"time"
)

// Router はブランチ名から接続先を解決する(branch.Manager が実装)。
type Router interface {
	// RouteBranch はブランチの接続先ポートを返す。存在しなければエラー。
	RouteBranch(ctx context.Context, name string) (port int, err error)
	// TouchConn は最終接続時刻を記録する(まとめ書き可)。
	TouchConn(name string)
}

// Config はプロキシの設定。
type Config struct {
	Listen           string // "0.0.0.0:3306"
	NamePattern      string
	BackendHost      string // 既定 127.0.0.1
	MaxConnPerBranch int    // 既定 50
	// AllowedUser が非空なら、<user>@<branch> の user 部がこれと一致する
	// 接続だけを受け付ける。認証前の第一関門。
	AllowedUser string

	// AppUser / AppPassword は方式A の app credential。クライアント認証の検証と
	// バックエンド接続の両方に使う(本番は Secrets Manager 由来の値を配線する)。
	AppUser     string
	AppPassword string

	// TLSConfig が非 nil なら TLS 終端を有効にする(クライアント↔sashiki)。
	TLSConfig *tls.Config
}

// Server は MySQL プロトコルプロキシ。
type Server struct {
	cfg    Config
	router Router
	nameRe *regexp.Regexp
	connID atomic.Uint32

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
		return fmt.Errorf("proxy listen: %w", err)
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
		log.Printf("proxy: %v", err)
	}
}

// authErr は ERR パケットを送って接続を切る。
func authErr(client net.Conn, seq byte, code uint16, state, msg string) error {
	_ = writePacket(client, packet{seq: seq, body: buildErr(code, state, msg)})
	return fmt.Errorf("auth rejected: %s", msg)
}

// authSwitchNative はクライアントへ mysql_native_password での再認証を要求し、
// 応答トークンとその seq を返す(caching_sha2 を話せない/native 明示の互換、#197)。
func (s *Server) authSwitchNative(client net.Conn, seq byte, salt []byte) (token []byte, newSeq byte, err error) {
	if err = writePacket(client, packet{seq: seq, body: buildAuthSwitchRequest(nativePlugin, salt)}); err != nil {
		return nil, seq, err
	}
	resp, err := readPacket(client)
	if err != nil {
		return nil, seq, err
	}
	// AuthSwitchResponse の body はそのまま native トークン(空パスワードは空)。
	return resp.body, resp.seq, nil
}

// authTerminate は方式A の認証フェーズ。クライアント認証を sashiki 自身が
// 検証し、成功してから branch を route / create してバックエンドへ接続し直す。
func (s *Server) authTerminate(ctx context.Context, client net.Conn) error {
	// 1. 合成ハンドシェイク送信(salt はクライアント検証に使う)
	hs, salt, err := buildInitialHandshake(s.connID.Add(1), s.cfg.TLSConfig != nil)
	if err != nil {
		return err
	}
	if err := writePacket(client, packet{seq: 0, body: hs}); err != nil {
		return err
	}

	// 2. クライアント応答。TLS 要求なら終端してから本応答を読み直す。
	resp, err := readPacket(client)
	if err != nil {
		return err
	}
	seq := resp.seq
	if isSSLRequest(resp.body) {
		if s.cfg.TLSConfig == nil {
			return authErr(client, seq+1, 1045, "28000", "TLS not configured")
		}
		tlsConn := tls.Server(client, s.cfg.TLSConfig)
		if err := tlsConn.Handshake(); err != nil {
			return fmt.Errorf("tls handshake: %w", err)
		}
		client = tlsConn
		if resp, err = readPacket(client); err != nil {
			return err
		}
		seq = resp.seq
	}
	hr, err := parseHandshakeResponse(resp.body)
	if err != nil {
		return authErr(client, seq+1, 1045, "28000", err.Error())
	}
	user, branch, ok := strings.Cut(hr.username, "@")
	if !ok || !s.nameRe.MatchString(branch) {
		return authErr(client, seq+1, 1045, "28000",
			fmt.Sprintf("Access denied: user must be <user>@<branch> (got %q)", hr.username))
	}
	if s.cfg.AllowedUser != "" && user != s.cfg.AllowedUser {
		return authErr(client, seq+1, 1045, "28000",
			fmt.Sprintf("Access denied for user %q", user))
	}

	// 3. 認証終端: app パスワードで検証する。**ここを通るまで branch に触れない**
	//    (認証前 lazy create の DoS 構造を解消 — #7 / #51)。
	//    既定は caching_sha2_password(広告に合わせる。MySQL 8.0/9.x 対応、#197)。
	//    sashiki は app パスワードを持つので fast-auth スクランブルを自前検証でき、
	//    成功時は AuthMoreData(0x03=fast_auth_success)を返してから OK を送る
	//    (TLS/RSA 不要)。native_password クライアントは従来どおり検証する。
	denied := func() error {
		return authErr(client, seq+1, 1045, "28000",
			fmt.Sprintf("Access denied for user '%s'@'%s' (using password: YES)", user, branch))
	}
	verified := false
	switch {
	case verifySHA2Password(s.cfg.AppPassword, salt, hr.authResp):
		// caching_sha2 の fast-auth 成功。fast_auth_success を通知してから OK。
		if err := writePacket(client, packet{seq: seq + 1, body: buildAuthMoreData(0x03)}); err != nil {
			return err
		}
		seq++
		verified = true
	case verifyNativePassword(s.cfg.AppPassword, salt, hr.authResp):
		// クライアントが native トークンを直接送ってきた(旧クライアント)。そのまま許可。
		verified = true
	case len(hr.authResp) == 0:
		// 空応答(クライアントが AuthSwitch を待っている:native を要求する CLI や
		// caching_sha2 を話せない古いクライアント)→ native へ切り替えて再認証する
		// (互換維持、#197)。非空トークンの不一致は下の denied で単なる誤 pw 扱い。
		tok, nseq, err := s.authSwitchNative(client, seq+1, salt)
		if err != nil {
			return err
		}
		seq = nseq
		verified = verifyNativePassword(s.cfg.AppPassword, salt, tok)
	}
	if !verified {
		// caching_sha2 を明示した非空トークンの不一致など:単なるパスワード誤り。
		return denied()
	}

	// 4. 認証済み → branch 解決(必要なら lazy create)
	port, err := s.router.RouteBranch(ctx, branch)
	if err != nil {
		return authErr(client, seq+1, 1049, "42000", fmt.Sprintf("Unknown branch '%s'", branch))
	}
	if !s.acquire(branch) {
		return authErr(client, seq+1, 1040, "08004", "Too many connections for branch")
	}
	defer s.release(branch)

	// 5. バックエンドへ sashiki 保持の credential で接続し直す
	backend, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", s.cfg.BackendHost, port), 10*time.Second)
	if err != nil {
		return authErr(client, seq+1, 2003, "HY000", "backend unavailable")
	}
	defer func() { _ = backend.Close() }()
	if berr := s.authenticateBackend(backend, hr.caps, hr.database); berr != nil {
		log.Printf("proxy: backend auth for %s@%s: %v", user, branch, berr)
		return authErr(client, seq+1, 2003, "HY000", "backend auth failed")
	}

	// 6. クライアントへ OK を返して認証完了
	if err := writePacket(client, packet{seq: seq + 1, body: buildOK()}); err != nil {
		return err
	}

	// 認証完了 → 素通し(接続終了までブロック)
	s.router.TouchConn(branch)
	enableKeepAlive(backend)
	_ = client.SetDeadline(time.Time{})
	pipe(client, backend)
	return nil
}

// closeWriter は書き込み側だけを閉じられる接続(*net.TCPConn / *tls.Conn)。
type closeWriter interface{ CloseWrite() error }

// pipe は認証後のデータフェーズを双方向に素通しする。片方向の EOF で
// 全体を叩き切るのではなく、その向きだけ CloseWrite で half-close して
// もう片方向を完走させる。これにより:
//   - backend が結果セットを流し込んでいる最中に client 側が先に終わっても、
//     結果を途中でぶった切らない
//   - backend(mysqld)が idle_stop_after で消えたときは client へ綺麗な EOF が
//     伝わり、プールしたコネクションを再利用するドライバが「半端に閉じた/残
//     バイトのある」接続を掴んで readColumns で panic するのを防ぐ
func pipe(client, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(backend, client) // client → backend(リクエスト)
		if cw, ok := backend.(closeWriter); ok {
			_ = cw.CloseWrite() // これ以上リクエストは来ない、と backend に伝える
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, backend) // backend → client(応答)
		if cw, ok := client.(closeWriter); ok {
			_ = cw.CloseWrite() // 応答完了/backend 消滅を client へ綺麗な EOF で伝える
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

// authenticateBackend は sashiki がクライアントとして backend mysqld へ
// mysql_native_password で認証する(方式A)。backend の dev は native_password。
func (s *Server) authenticateBackend(backend net.Conn, clientCaps uint32, database string) error {
	bhs, err := readPacket(backend)
	if err != nil {
		return fmt.Errorf("read backend handshake: %w", err)
	}
	salt, backendCaps, err := parseBackendHandshake(bhs.body)
	if err != nil {
		return err
	}
	// backend への capability 申告は「クライアントが実際に交渉した capability」に
	// 合わせる(#125)。特に DEPRECATE_EOF を揃えないと、backend が deprecate 形式の
	// 結果セット(中間 EOF 無し・末尾 OK)を返し、DEPRECATE_EOF を立てないドライバ
	// (PHP mysqlnd / Node 等)が読めず結果が空/エラーになる。以前は synthCaps を
	// 固定申告していたため、非 deprecate クライアントで壊れていた。
	//
	// DB 選択(capConnectWithDB)だけは sashiki 側で制御する: クライアントが接続時に
	// 指定した DB を backend にも引き継ぐ(go-sql-driver 等は handshake の database
	// フィールドでのみ DB を選ぶため、転送しないと "No database selected" になる)。
	hr := handshakeResponse{maxLen: 16 * 1024 * 1024, charset: 0xff}
	hr.caps = clientCaps & synthCaps
	if database != "" {
		hr.caps |= capConnectWithDB
		hr.database = database
	} else {
		hr.caps &^= capConnectWithDB
	}
	token := nativeToken(s.cfg.AppPassword, salt)
	resp := buildBackendHandshakeResponse(hr, backendCaps, s.cfg.AppUser, token, nativePlugin)
	if err := writePacket(backend, packet{seq: bhs.seq + 1, body: resp}); err != nil {
		return err
	}
	for {
		p, err := readPacket(backend)
		if err != nil {
			return fmt.Errorf("read backend auth result: %w", err)
		}
		switch {
		case isOK(p.body):
			return nil
		case isErr(p.body):
			return fmt.Errorf("backend rejected app credential: %s", errText(p.body))
		case len(p.body) > 0 && p.body[0] == 0xfe: // AuthSwitchRequest → native で応答し直す
			swSalt := parseAuthSwitchSalt(p.body)
			tok := nativeToken(s.cfg.AppPassword, swSalt)
			if err := writePacket(backend, packet{seq: p.seq + 1, body: tok}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected backend auth packet 0x%02x", p.body[0])
		}
	}
}

// parseAuthSwitchSalt は AuthSwitchRequest(0xfe + plugin\0 + salt)から salt を取る。
func parseAuthSwitchSalt(body []byte) []byte {
	pos := 1
	for pos < len(body) && body[pos] != 0 { // plugin name
		pos++
	}
	pos++ // null
	salt := body[pos:]
	for len(salt) > 0 && salt[len(salt)-1] == 0 { // 末尾 null を落とす
		salt = salt[:len(salt)-1]
	}
	return salt
}
