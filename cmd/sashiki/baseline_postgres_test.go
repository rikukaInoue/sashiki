package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rikukadev/sashiki/internal/config"
)

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"dev":       `"dev"`,
		"my role":   `"my role"`,
		`we"ird`:    `"we""ird"`,
		"drop\"tbl": `"drop""tbl"`,
	}
	for in, want := range cases {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"pw":       `'pw'`,
		"it's":     `'it''s'`,
		"a'; DROP": `'a''; DROP'`,
	}
	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(%q) = %s, want %s", in, got, want)
		}
	}
}

// pg_dump のカスタム形式は先頭 "PGDMP" で判定する。プレーン SQL と取り違えると
// psql に流して壊れるので、ここが崩れると import が静かに失敗する。
func TestIsPgCustomDump(t *testing.T) {
	dir := t.TempDir()

	custom := filepath.Join(dir, "custom.dump")
	if err := os.WriteFile(custom, append([]byte("PGDMP"), 0x01, 0x02), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isPgCustomDump(custom) {
		t.Error("PGDMP で始まるファイルはカスタム形式と判定されるべき")
	}

	plain := filepath.Join(dir, "plain.sql")
	if err := os.WriteFile(plain, []byte("CREATE TABLE t (id int);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isPgCustomDump(plain) {
		t.Error("プレーン SQL はカスタム形式ではない")
	}

	// 5 byte 未満でも落ちないこと
	tiny := filepath.Join(dir, "tiny.sql")
	if err := os.WriteFile(tiny, []byte("--"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isPgCustomDump(tiny) {
		t.Error("短いファイルはカスタム形式ではない")
	}

	if isPgCustomDump(filepath.Join(dir, "missing")) {
		t.Error("存在しないファイルは false")
	}
}

func TestPgBinPrefersBinDir(t *testing.T) {
	dir := t.TempDir()
	psql := filepath.Join(dir, "psql")
	if err := os.WriteFile(psql, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := pgBin(dir, "psql"); got != psql {
		t.Errorf("pgBin = %q, want %q", got, psql)
	}
	// bin_dir に無ければ PATH 解決に任せて素の名前を返す
	if got := pgBin(dir, "pg_restore"); got != "pg_restore" {
		t.Errorf("pgBin fallback = %q, want pg_restore", got)
	}
	if got := pgBin("", "initdb"); got != "initdb" {
		t.Errorf("pgBin with empty bin_dir = %q, want initdb", got)
	}
}

// apfs / reflink は #227 でローカル CoW 経路に振り分けるようになった。
// storage.local.root 未設定なら、その旨が分かるエラーで止まること
// (zfs 経路へ落ちて "dataset が見つかりません" になると原因が分からない)。
func TestPostgresImportUsesLocalPathForCoWBackends(t *testing.T) {
	for _, backend := range []string{"apfs", "reflink"} {
		cfg := config.Config{}
		cfg.Engine.Type = "postgres"
		cfg.Storage.Backend = backend
		err := runPostgresBaselineImport(cfg, baselineImportOpts{})
		if err == nil {
			t.Fatalf("backend %s: root 未設定なのでエラーになるべき", backend)
		}
		if !contains(err.Error(), "storage.local.root") {
			t.Errorf("backend %s: ローカル経路のエラーになるべき: %v", backend, err)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
