// Package proxy は :3306 の固定エンドポイント。クライアントのユーザー名
// `<user>@<branch>` からバックエンドの mysqld を選び、認証を中継する。
//
// sashiki はパスワードを保存しない。合成ハンドシェイクでユーザー名だけを取得し、
// バックエンドにはユーザーの実プラグインと異なるプラグインを名乗って接続する。
// するとバックエンドが AuthSwitchRequest(新しい salt 付き)を返すので、それを
// そのままクライアントへ転送して認証させる(正否の判断はバックエンド)。
// この方式では両側の sequence 番号が自然に揃い、書き換えが不要(ADR-006)。
// 認証完了後は素通し。
package proxy

import (
	"context"
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
	// 接続だけを受け付ける。認証前の lazy create の乱発を抑える
	// (name_pattern・max_branches・メモリガードに加えた第一関門)。
	AllowedUser string
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
	_ = client.SetDeadline(time.Now().Add(60 * time.Second))

	if err := s.authRelay(ctx, client); err != nil {
		log.Printf("proxy: %v", err)
	}
}

// authErr は ERR パケットを送って接続を切る。
func authErr(client net.Conn, seq byte, code uint16, state, msg string) error {
	_ = writePacket(client, packet{seq: seq, body: buildErr(code, state, msg)})
	return fmt.Errorf("auth rejected: %s", msg)
}

// authRelay は認証フェーズを中継し、成功後はそのまま素通しに移行して
// 接続が終わるまでブロックする。
func (s *Server) authRelay(ctx context.Context, client net.Conn) error {
	// 1. 合成ハンドシェイク送信
	hs, err := buildInitialHandshake(s.connID.Add(1))
	if err != nil {
		return err
	}
	if err := writePacket(client, packet{seq: 0, body: hs}); err != nil {
		return err
	}

	// 2. クライアント応答からユーザー名を取得
	resp, err := readPacket(client)
	if err != nil {
		return err
	}
	hr, err := parseHandshakeResponse(resp.body)
	if err != nil {
		return authErr(client, resp.seq+1, 1045, "28000", err.Error())
	}
	user, branch, ok := strings.Cut(hr.username, "@")
	if !ok || !s.nameRe.MatchString(branch) {
		return authErr(client, resp.seq+1, 1045, "28000",
			fmt.Sprintf("Access denied: user must be <user>@<branch> (got %q)", hr.username))
	}
	if s.cfg.AllowedUser != "" && user != s.cfg.AllowedUser {
		return authErr(client, resp.seq+1, 1045, "28000",
			fmt.Sprintf("Access denied for user %q", user))
	}

	// 3. ブランチ解決
	port, err := s.router.RouteBranch(ctx, branch)
	if err != nil {
		return authErr(client, resp.seq+1, 1049, "42000",
			fmt.Sprintf("Unknown branch '%s'", branch))
	}
	if !s.acquire(branch) {
		return authErr(client, resp.seq+1, 1040, "08004", "Too many connections for branch")
	}
	defer s.release(branch)

	// 4. バックエンド接続 + salt 取得
	backend, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", s.cfg.BackendHost, port), 10*time.Second)
	if err != nil {
		return authErr(client, resp.seq+1, 2003, "HY000", "backend unavailable")
	}
	defer func() { _ = backend.Close() }()
	bhs, err := readPacket(backend)
	if err != nil {
		return err
	}
	_, backendCaps, err := parseBackendHandshake(bhs.body)
	if err != nil {
		return err
	}

	// 5. バックエンドへ HandshakeResponse。ユーザーの実プラグイン(native)と
	//    異なる caching_sha2 を名乗ることで、バックエンドに AuthSwitchRequest を
	//    送らせる。それをそのままクライアントへ転送すると、両側の sequence が
	//    自然に揃う(handshake=0, response=1, switch=2, reply=3, result=4)ため
	//    seq の書き換えが不要になる。ADR-006。
	bResp := buildBackendHandshakeResponse(hr, backendCaps, user, nil, sha2Plugin)
	if err := writePacket(backend, packet{seq: 1, body: bResp}); err != nil {
		return err
	}

	// 6. 認証フェーズの素直な転送(seq もそのまま)。backend の OK/ERR で終わる。
	for {
		p, err := readPacket(backend)
		if err != nil {
			return fmt.Errorf("read backend auth reply: %w", err)
		}
		if err := writePacket(client, p); err != nil {
			return err
		}
		if isOK(p.body) {
			break
		}
		if isErr(p.body) {
			return fmt.Errorf("backend rejected auth for %s@%s", user, branch)
		}
		cp, err := readPacket(client)
		if err != nil {
			return fmt.Errorf("read client auth reply: %w", err)
		}
		if err := writePacket(backend, cp); err != nil {
			return err
		}
	}

	// 認証完了 → 素通し(接続終了までブロック)
	s.router.TouchConn(branch)
	_ = client.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, backend); done <- struct{}{} }()
	<-done // 片方向が終わったら両方閉じる(defer が client/backend を閉じる)
	return nil
}
