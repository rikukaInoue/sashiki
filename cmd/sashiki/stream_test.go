package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsS3AndStdio(t *testing.T) {
	if !isS3("s3://bucket/key") || isS3("/tmp/x.sql") || isS3("-") {
		t.Error("isS3 の判定が誤り")
	}
	if !isStdio("-") || isStdio("s3://b/k") || isStdio("/tmp/-") {
		t.Error("isStdio の判定が誤り")
	}
}

func TestOpenSourceLocalFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(p, []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := openSource(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "SELECT 1;\n" {
		t.Errorf("read = %q", b)
	}
}

func TestCreateSinkLocalFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.bin")
	w, err := createSink(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("stream")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "stream" {
		t.Errorf("written = %q", b)
	}
}

// ローカルパスは materialize でコピーされず、そのまま返ること
// (14GB のダンプを無駄に複製しないため)。
func TestMaterializeKeepsLocalPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, cleanup, err := materialize(p, "sashiki-*")
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Errorf("materialize = %q, want %q (コピーしないこと)", got, p)
	}
}

// aws CLI が無い環境では s3:// をその場で分かるエラーにすること。
func TestS3RequiresAWSCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // aws が見つからない PATH
	if _, err := openSource("s3://bucket/key"); err == nil {
		t.Fatal("aws CLI が無ければエラーになるべき")
	} else if !strings.Contains(err.Error(), "aws CLI") {
		t.Errorf("エラーに理由が要る: %v", err)
	}
	if _, err := createSink("s3://bucket/key"); err == nil {
		t.Fatal("aws CLI が無ければエラーになるべき")
	}
}
