package pgproxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// PostgreSQL フロントエンド/バックエンドプロトコル v3 の最小実装。
// 必要なのは「認証フェーズを終端し、その後は素通しする」ぶんだけなので、
// StartupMessage・認証系・ErrorResponse のみを扱う。
//
// バイト順はすべてビッグエンディアン。StartupMessage だけ型バイトが無く
// (Int32 長 + Int32 コード)、それ以降のメッセージは 型 1 byte + Int32 長 + 本体。
// 長さフィールドは自分自身の 4 byte を含み、型バイトは含まない。

const (
	// 特殊な StartupMessage のコード(protocol version の位置に入る)。
	codeSSLRequest    = 80877103 // 1234<<16 | 5679
	codeGSSENCRequest = 80877104 // 1234<<16 | 5680
	codeCancelRequest = 80877102 // 1234<<16 | 5678
	protocolV3        = 196608   // 3<<16
)

// メッセージ型(サーバ→クライアント)。
const (
	msgAuthentication = 'R'
	msgErrorResponse  = 'E'
	msgReadyForQuery  = 'Z'
)

// メッセージ型(クライアント→サーバ)。PasswordMessage / SASLInitialResponse /
// SASLResponse はすべて 'p' で、文脈で区別する。
const msgPassword = 'p'

// 認証サブコード。
const (
	authOK                = 0
	authCleartextPassword = 3
	authMD5Password       = 5
	authSASL              = 10
	authSASLContinue      = 11
	authSASLFinal         = 12
)

// startup はクライアントの最初のメッセージ。
type startup struct {
	code   int32             // protocolV3 / codeSSLRequest / codeCancelRequest ...
	params map[string]string // protocolV3 のときのみ
}

// readStartup は StartupMessage(型バイト無し)を読む。
func readStartup(c net.Conn) (startup, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		return startup{}, fmt.Errorf("read startup length: %w", err)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	// 長さは自分自身を含む。最低 8 byte(長さ + コード)。
	if n < 8 || n > 1<<20 {
		return startup{}, fmt.Errorf("startup length %d out of range", n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return startup{}, fmt.Errorf("read startup body: %w", err)
	}
	s := startup{code: int32(binary.BigEndian.Uint32(body[:4]))}
	if s.code != protocolV3 {
		return s, nil // SSLRequest / CancelRequest 等はパラメータを持たない
	}
	s.params = map[string]string{}
	rest := body[4:]
	for len(rest) > 0 && rest[0] != 0 {
		k, r, err := cstring(rest)
		if err != nil {
			return s, err
		}
		v, r2, err := cstring(r)
		if err != nil {
			return s, err
		}
		s.params[k] = v
		rest = r2
	}
	return s, nil
}

// cstring は null 終端文字列を 1 個取り出し、残りを返す。
func cstring(b []byte) (string, []byte, error) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:], nil
		}
	}
	return "", nil, fmt.Errorf("unterminated string in startup message")
}

// message は型付きメッセージ。
type message struct {
	typ  byte
	body []byte // 長さフィールドを除いた本体
}

// readMessage は型付きメッセージを 1 個読む。バッファを持たず必要なぶんだけ
// 読むので、認証後に生の io.Copy へ切り替えてもバイトを取りこぼさない。
func readMessage(c net.Conn) (message, error) {
	var head [5]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return message{}, err
	}
	n := binary.BigEndian.Uint32(head[1:5])
	if n < 4 || n > 1<<24 {
		return message{}, fmt.Errorf("message length %d out of range", n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return message{}, err
	}
	return message{typ: head[0], body: body}, nil
}

// writeMessage は型付きメッセージを書く。
func writeMessage(c net.Conn, typ byte, body []byte) error {
	buf := make([]byte, 5+len(body))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(body)))
	copy(buf[5:], body)
	_, err := c.Write(buf)
	return err
}

// writeStartup は StartupMessage(型バイト無し)を書く。バックエンドへ接続する
// ときに使う。
func writeStartup(c net.Conn, params map[string]string) error {
	body := make([]byte, 4, 64)
	binary.BigEndian.PutUint32(body[:4], protocolV3)
	for k, v := range params {
		body = append(body, k...)
		body = append(body, 0)
		body = append(body, v...)
		body = append(body, 0)
	}
	body = append(body, 0) // パラメータ列の終端
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf[:4], uint32(4+len(body)))
	copy(buf[4:], body)
	_, err := c.Write(buf)
	return err
}

// authCode は Authentication メッセージのサブコードを返す。
func authCode(body []byte) (int32, []byte, bool) {
	if len(body) < 4 {
		return 0, nil, false
	}
	return int32(binary.BigEndian.Uint32(body[:4])), body[4:], true
}

// buildAuthOK は AuthenticationOk の本体を返す。
func buildAuthOK() []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, authOK)
	return b
}

// buildAuthMessage は Authentication メッセージの本体(サブコード + データ)を作る。
func buildAuthMessage(code int32, data []byte) []byte {
	b := make([]byte, 4, 4+len(data))
	binary.BigEndian.PutUint32(b, uint32(code))
	return append(b, data...)
}

// buildError は ErrorResponse の本体を作る。severity/SQLSTATE/message の
// 最小構成(フィールドは 1 byte の識別子 + null 終端文字列、全体を null で終端)。
func buildError(sqlstate, msg string) []byte {
	b := []byte{'S'}
	b = append(b, "FATAL"...)
	b = append(b, 0, 'V')
	b = append(b, "FATAL"...)
	b = append(b, 0, 'C')
	b = append(b, sqlstate...)
	b = append(b, 0, 'M')
	b = append(b, msg...)
	b = append(b, 0, 0)
	return b
}

// errorText は ErrorResponse から 'M'(人間向けメッセージ)を取り出す。
func errorText(body []byte) string {
	rest := body
	for len(rest) > 0 && rest[0] != 0 {
		code := rest[0]
		v, r, err := cstring(rest[1:])
		if err != nil {
			return "malformed error response"
		}
		if code == 'M' {
			return v
		}
		rest = r
	}
	return "unknown error"
}
