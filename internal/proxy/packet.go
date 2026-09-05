// MySQL プロトコルのパケット読み書きと、認証フェーズに必要な最小限の
// パース/構築。認証完了後は素通しするため、ここで扱うのは
// handshake / handshake response / auth switch / OK / ERR のみ。
package proxy

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

// capability flags (使う分だけ)
const (
	capLongPassword    = 0x00000001
	capProtocol41      = 0x00000200
	capSSL             = 0x00000800
	capTransactions    = 0x00002000
	capSecureConn      = 0x00008000
	capPluginAuth      = 0x00080000
	capConnectAttrs    = 0x00100000
	capPluginAuthLenC  = 0x00200000
	capDeprecateEOF    = 0x01000000
	capConnectWithDB   = 0x00000008
	capLocalFiles      = 0x00000080
	capMultiStatements = 0x00010000
	capMultiResults    = 0x00020000
)

const (
	nativePlugin = "mysql_native_password"
	sha2Plugin   = "caching_sha2_password"
)

// packet は 1 パケット(ヘッダ除くペイロード + シーケンス番号)。
type packet struct {
	seq  byte
	body []byte
}

func readPacket(r io.Reader) (packet, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return packet{}, err
	}
	length := int(head[0]) | int(head[1])<<8 | int(head[2])<<16
	if length > 16*1024*1024 {
		return packet{}, fmt.Errorf("packet too large: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return packet{}, err
	}
	return packet{seq: head[3], body: body}, nil
}

func writePacket(w io.Writer, p packet) error {
	head := []byte{
		byte(len(p.body)), byte(len(p.body) >> 8), byte(len(p.body) >> 16), p.seq,
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	_, err := w.Write(p.body)
	return err
}

// synthCaps は twig が合成ハンドシェイクで広告する capability。
// クライアントはこの範囲でしか機能を使わないため、バックエンドへの申告も
// 必ずこの範囲に絞る(広告していない機能を申告すると、クライアントが送る
// パケットとバックエンドの期待がずれて Malformed packet になる)。
const synthCaps = uint32(capLongPassword | capProtocol41 | capSecureConn | capPluginAuth |
	capPluginAuthLenC | capTransactions | capConnectWithDB | capDeprecateEOF |
	capMultiStatements | capMultiResults)

// buildInitialHandshake は twig が名乗る合成ハンドシェイク(protocol 10)。
// salt は使い捨て(クライアントの応答は捨てて AuthSwitch でやり直させる)。
func buildInitialHandshake(connID uint32) ([]byte, error) {
	salt := make([]byte, 20)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	// salt に 0x00 が混ざると null 終端と衝突するため避ける
	for i := range salt {
		salt[i] = salt[i]%94 + 33
	}
	caps := synthCaps

	b := []byte{10}                                 // protocol version
	b = append(b, []byte("8.0.0-twig-proxy")...)    // server version
	b = append(b, 0)                                // null terminator
	b = binary.LittleEndian.AppendUint32(b, connID) // thread id
	b = append(b, salt[:8]...)                      // auth-plugin-data part1
	b = append(b, 0)                                // filler
	b = append(b, byte(caps), byte(caps>>8))        // capabilities low
	b = append(b, 0xff)                             // charset (utf8mb4)
	b = append(b, 0x02, 0x00)                       // status flags
	b = append(b, byte(caps>>16), byte(caps>>24))   // capabilities high
	b = append(b, 21)                               // auth plugin data len (20+1)
	b = append(b, make([]byte, 10)...)              // reserved
	b = append(b, salt[8:20]...)                    // auth-plugin-data part2
	b = append(b, 0)
	b = append(b, []byte(nativePlugin)...)
	b = append(b, 0)
	return b, nil
}

// handshakeResponse は client の HandshakeResponse41 のうち必要な項目。
type handshakeResponse struct {
	caps     uint32
	maxLen   uint32
	charset  byte
	username string
	database string
}

func parseHandshakeResponse(body []byte) (handshakeResponse, error) {
	var r handshakeResponse
	if len(body) < 32 {
		return r, fmt.Errorf("handshake response too short")
	}
	r.caps = binary.LittleEndian.Uint32(body[0:4])
	if r.caps&capProtocol41 == 0 {
		return r, fmt.Errorf("client does not speak protocol 4.1")
	}
	if r.caps&capSSL != 0 {
		return r, fmt.Errorf("TLS is not supported by twig proxy yet")
	}
	r.maxLen = binary.LittleEndian.Uint32(body[4:8])
	r.charset = body[8]
	pos := 32 // 4+4+1+23
	// username (null 終端)
	end := pos
	for end < len(body) && body[end] != 0 {
		end++
	}
	if end >= len(body) {
		return r, fmt.Errorf("username not terminated")
	}
	r.username = string(body[pos:end])
	pos = end + 1
	// auth response (読み飛ばす — twig の salt に対する応答なので使わない)
	if r.caps&capPluginAuthLenC != 0 {
		if pos >= len(body) {
			return r, fmt.Errorf("truncated auth data")
		}
		alen := int(body[pos])
		pos++
		pos += alen
	} else if r.caps&capSecureConn != 0 {
		if pos >= len(body) {
			return r, fmt.Errorf("truncated auth data")
		}
		alen := int(body[pos])
		pos++
		pos += alen
	} else {
		for pos < len(body) && body[pos] != 0 {
			pos++
		}
		pos++
	}
	if pos > len(body) {
		return r, fmt.Errorf("truncated packet")
	}
	// database (CLIENT_CONNECT_WITH_DB)
	if r.caps&capConnectWithDB != 0 && pos < len(body) {
		end = pos
		for end < len(body) && body[end] != 0 {
			end++
		}
		r.database = string(body[pos:end])
	}
	return r, nil
}

// buildAuthSwitchRequest はバックエンドの salt でクライアントに認証やり直しを求める。
func buildAuthSwitchRequest(salt []byte) []byte {
	b := []byte{0xfe}
	b = append(b, []byte(nativePlugin)...)
	b = append(b, 0)
	b = append(b, salt...)
	b = append(b, 0)
	return b
}

// parseBackendHandshake はバックエンド mysqld のハンドシェイクから salt と caps を取る。
func parseBackendHandshake(body []byte) (salt []byte, caps uint32, err error) {
	if len(body) < 1 || body[0] != 10 {
		return nil, 0, fmt.Errorf("unexpected protocol version")
	}
	pos := 1
	for pos < len(body) && body[pos] != 0 { // server version
		pos++
	}
	pos++    // null
	pos += 4 // thread id
	if pos+8 > len(body) {
		return nil, 0, fmt.Errorf("short handshake")
	}
	salt = append(salt, body[pos:pos+8]...)
	pos += 8
	pos++ // filler
	if pos+2 > len(body) {
		return nil, 0, fmt.Errorf("short handshake")
	}
	caps = uint32(binary.LittleEndian.Uint16(body[pos : pos+2]))
	pos += 2
	if pos+1+2+2+1+10 <= len(body) {
		pos++    // charset
		pos += 2 // status
		caps |= uint32(binary.LittleEndian.Uint16(body[pos:pos+2])) << 16
		pos += 2
		authLen := int(body[pos])
		pos++
		pos += 10 // reserved
		// salt part2: max(13, authLen-8) だが末尾 null を除いた 12 byte が慣例
		rest := 12
		if authLen > 0 && authLen-8-1 < rest {
			rest = authLen - 8 - 1
		}
		if pos+rest <= len(body) {
			salt = append(salt, body[pos:pos+rest]...)
		}
	}
	return salt, caps, nil
}

// buildBackendHandshakeResponse はバックエンドへ送る HandshakeResponse41。
// plugin にユーザーの実プラグインと異なる名前を渡すと、バックエンドは必ず
// AuthSwitchRequest を返す(proxy はこれを利用する)。
func buildBackendHandshakeResponse(r handshakeResponse, backendCaps uint32, user string, authResp []byte, plugin string) []byte {
	caps := r.caps & backendCaps & synthCaps
	caps |= capProtocol41 | capSecureConn | capPluginAuth

	b := binary.LittleEndian.AppendUint32(nil, caps)
	b = binary.LittleEndian.AppendUint32(b, r.maxLen)
	b = append(b, r.charset)
	b = append(b, make([]byte, 23)...)
	b = append(b, []byte(user)...)
	b = append(b, 0)
	b = append(b, byte(len(authResp)))
	b = append(b, authResp...)
	if caps&capConnectWithDB != 0 && r.database != "" {
		b = append(b, []byte(r.database)...)
		b = append(b, 0)
	}
	b = append(b, []byte(plugin)...)
	b = append(b, 0)
	return b
}

// buildErr は ERR パケットを作る(認証前フェーズ用)。
func buildErr(code uint16, sqlState, msg string) []byte {
	b := []byte{0xff}
	b = binary.LittleEndian.AppendUint16(b, code)
	b = append(b, '#')
	b = append(b, []byte(sqlState)...)
	b = append(b, []byte(msg)...)
	return b
}

func isOK(body []byte) bool  { return len(body) > 0 && body[0] == 0x00 }
func isErr(body []byte) bool { return len(body) > 0 && body[0] == 0xff }
