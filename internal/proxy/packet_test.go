package proxy

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestPacketRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := packet{seq: 3, body: []byte("hello")}
	if err := writePacket(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := readPacket(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.seq != 3 || string(out.body) != "hello" {
		t.Errorf("out = %+v", out)
	}
}

func TestInitialHandshakeParsableAsBackend(t *testing.T) {
	// 自前の合成ハンドシェイクを parseBackendHandshake で読めること
	// (フォーマットの自己整合性チェック)。
	hs, hsSalt, err := buildInitialHandshake(42, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hsSalt) != 20 {
		t.Errorf("returned salt len = %d, want 20", len(hsSalt))
	}
	salt, caps, err := parseBackendHandshake(hs)
	if err != nil {
		t.Fatal(err)
	}
	if len(salt) != 20 {
		t.Errorf("salt len = %d, want 20", len(salt))
	}
	if caps&capProtocol41 == 0 || caps&capPluginAuth == 0 {
		t.Errorf("caps = %x", caps)
	}
}

func buildClientResponse(t *testing.T, caps uint32, username, db string, auth []byte) []byte {
	t.Helper()
	b := binary.LittleEndian.AppendUint32(nil, caps)
	b = binary.LittleEndian.AppendUint32(b, 1<<24)
	b = append(b, 0xff)
	b = append(b, make([]byte, 23)...)
	b = append(b, []byte(username)...)
	b = append(b, 0)
	b = append(b, byte(len(auth)))
	b = append(b, auth...)
	if caps&capConnectWithDB != 0 && db != "" {
		b = append(b, []byte(db)...)
		b = append(b, 0)
	}
	b = append(b, []byte(nativePlugin)...)
	b = append(b, 0)
	return b
}

func TestParseHandshakeResponse(t *testing.T) {
	caps := uint32(capProtocol41 | capSecureConn | capPluginAuth | capPluginAuthLenC | capConnectWithDB)
	body := buildClientResponse(t, caps, "dev@pr-123", "app", bytes.Repeat([]byte{0xab}, 20))
	hr, err := parseHandshakeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if hr.username != "dev@pr-123" {
		t.Errorf("username = %q", hr.username)
	}
	if hr.database != "app" {
		t.Errorf("database = %q", hr.database)
	}
}

func TestParseHandshakeResponseRejectsOldProtocol(t *testing.T) {
	// 非 4.1 は拒否(SSL は authTerminate 側で isSSLRequest により終端するため
	// parseHandshakeResponse では拒否しない)。
	body := buildClientResponse(t, capSecureConn, "dev@pr-1", "", nil)
	if _, err := parseHandshakeResponse(body); err == nil {
		t.Error("non-4.1 should be rejected")
	}
}

func TestVerifyNativePassword(t *testing.T) {
	salt := bytes.Repeat([]byte{0x21}, 20)
	// クライアントは同じ salt でトークンを計算する。正しいパスワードは検証成功。
	tok := nativeToken("s3cret", salt)
	if !verifyNativePassword("s3cret", salt, tok) {
		t.Error("correct password should verify")
	}
	// 誤ったパスワードは失敗。
	if verifyNativePassword("wrong", salt, tok) {
		t.Error("wrong password must not verify")
	}
	// 空パスワード同士は空トークンで一致。
	if !verifyNativePassword("", salt, nativeToken("", salt)) {
		t.Error("empty password should verify against empty token")
	}
	// 空トークン(認証情報なし)は非空パスワードに対して失敗。
	if verifyNativePassword("s3cret", salt, nil) {
		t.Error("empty token must not verify against a real password")
	}
}

func TestIsSSLRequest(t *testing.T) {
	// SSLRequest: caps に SSL、username 無しの短いパケット。
	ssl := binary.LittleEndian.AppendUint32(nil, uint32(capProtocol41|capSSL))
	ssl = binary.LittleEndian.AppendUint32(ssl, 1<<24)
	ssl = append(ssl, 0xff)
	ssl = append(ssl, make([]byte, 23)...)
	if !isSSLRequest(ssl) {
		t.Error("short SSL packet should be detected")
	}
	// username 付きのフル応答は SSLRequest ではない。
	full := buildClientResponse(t, capProtocol41|capSSL|capSecureConn, "dev@pr-1", "", bytes.Repeat([]byte{1}, 20))
	if isSSLRequest(full) {
		t.Error("full handshake response is not an SSL request")
	}
}

func TestBackendHandshakeResponseKeepsUserAndDB(t *testing.T) {
	hr := handshakeResponse{
		caps:     capProtocol41 | capSecureConn | capPluginAuth | capConnectWithDB,
		maxLen:   1 << 24,
		charset:  0xff,
		username: "dev@pr-9",
		database: "app",
	}
	auth := bytes.Repeat([]byte{0xcd}, 20)
	body := buildBackendHandshakeResponse(hr, hr.caps, "dev", auth, sha2Plugin)
	parsed, err := parseHandshakeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.username != "dev" {
		t.Errorf("username = %q, want dev (branch stripped)", parsed.username)
	}
	if parsed.database != "app" {
		t.Errorf("database = %q", parsed.database)
	}
}

func TestAuthSwitchRequestFormat(t *testing.T) {
	salt := bytes.Repeat([]byte{0x01}, 20)
	b := buildAuthSwitchRequest(salt)
	if b[0] != 0xfe {
		t.Errorf("first byte = %x, want 0xfe", b[0])
	}
	if !bytes.Contains(b, []byte(nativePlugin)) {
		t.Error("plugin name missing")
	}
	if b[len(b)-1] != 0 {
		t.Error("should end with null")
	}
}

func TestErrPacket(t *testing.T) {
	b := buildErr(1049, "42000", "Unknown branch 'x'")
	if !isErr(b) {
		t.Error("should be ERR")
	}
	if isOK(b) {
		t.Error("should not be OK")
	}
	if binary.LittleEndian.Uint16(b[1:3]) != 1049 {
		t.Error("bad code")
	}
}
