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
	hs, err := buildInitialHandshake(42)
	if err != nil {
		t.Fatal(err)
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

func TestParseHandshakeResponseRejectsSSLAndOldProtocol(t *testing.T) {
	body := buildClientResponse(t, capProtocol41|capSSL|capPluginAuthLenC, "dev@pr-1", "", nil)
	if _, err := parseHandshakeResponse(body); err == nil {
		t.Error("SSL should be rejected")
	}
	body = buildClientResponse(t, capSecureConn, "dev@pr-1", "", nil)
	if _, err := parseHandshakeResponse(body); err == nil {
		t.Error("non-4.1 should be rejected")
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
