package pgproxy

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
)

// authenticateBackend は sashiki がクライアントとして backend postgres へ
// 接続し直し、AuthenticationOk までを消費する。**AuthenticationOk の直後で
// 止める**のが重要で、続く ParameterStatus / BackendKeyData / ReadyForQuery は
// 読まずに残し、そのまま素通しでクライアントへ流す(クライアントは自分が
// 期待する起動シーケンスをバックエンドから直接受け取る形になる)。
//
// pg_hba.conf の設定によって trust / password / md5 / scram-sha-256 の
// いずれかが来るので、すべてに応答できるようにしておく。
func authenticateBackend(backend net.Conn, user, database, password string) error {
	params := map[string]string{"user": user}
	if database != "" {
		params["database"] = database
	}
	if err := writeStartup(backend, params); err != nil {
		return fmt.Errorf("send startup: %w", err)
	}

	var sc *scramClient
	for {
		m, err := readMessage(backend)
		if err != nil {
			return fmt.Errorf("read backend message: %w", err)
		}
		switch m.typ {
		case msgErrorResponse:
			return fmt.Errorf("backend rejected: %s", errorText(m.body))
		case msgAuthentication:
			code, data, ok := authCode(m.body)
			if !ok {
				return fmt.Errorf("malformed authentication message")
			}
			switch code {
			case authOK:
				return nil // ここで止める(後続は素通しに任せる)
			case authCleartextPassword:
				if err := writePassword(backend, password); err != nil {
					return err
				}
			case authMD5Password:
				if len(data) < 4 {
					return fmt.Errorf("md5 salt is missing")
				}
				if err := writePassword(backend, md5Token(password, user, data[:4])); err != nil {
					return err
				}
			case authSASL:
				sc = &scramClient{password: password}
				first, err := sc.first()
				if err != nil {
					return err
				}
				if err := writeSASLInitial(backend, "SCRAM-SHA-256", first); err != nil {
					return err
				}
			case authSASLContinue:
				if sc == nil {
					return fmt.Errorf("unexpected SASLContinue")
				}
				final, err := sc.final(string(data))
				if err != nil {
					return err
				}
				if err := writeMessage(backend, msgPassword, []byte(final)); err != nil {
					return err
				}
			case authSASLFinal:
				if sc == nil {
					return fmt.Errorf("unexpected SASLFinal")
				}
				if err := sc.verifyServerFinal(string(data)); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unsupported backend auth method %d", code)
			}
		default:
			return fmt.Errorf("unexpected backend message %q during auth", m.typ)
		}
	}
}

// writePassword は PasswordMessage('p' + null 終端文字列)を送る。
func writePassword(c net.Conn, s string) error {
	body := append([]byte(s), 0)
	return writeMessage(c, msgPassword, body)
}

// writeSASLInitial は SASLInitialResponse(メカニズム名 + Int32 長 + データ)を送る。
func writeSASLInitial(c net.Conn, mechanism, data string) error {
	body := append([]byte(mechanism), 0)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(data)))
	body = append(body, n[:]...)
	body = append(body, data...)
	return writeMessage(c, msgPassword, body)
}

// md5Token は PostgreSQL の md5 認証応答を作る:
// "md5" + hex(md5(hex(md5(password + user)) + salt))
func md5Token(password, user string, salt []byte) string {
	inner := md5.Sum([]byte(password + user))
	outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), salt...))
	return "md5" + hex.EncodeToString(outer[:])
}
