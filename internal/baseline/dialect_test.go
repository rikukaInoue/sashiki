package baseline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PostgreSQL は接続中の DB を跨いだ問い合わせができないため、適用記録は
// 専用 DB ではなく対象 DB 内のスキーマに置く。方言がそれを表していること。
func TestPostgresDialectUsesSchema(t *testing.T) {
	d := PostgresDialect()
	if !strings.Contains(d.CreateMeta, "CREATE SCHEMA") {
		t.Errorf("postgres は CREATE SCHEMA を使うべき: %q", d.CreateMeta)
	}
	if strings.Contains(d.CreateMigrations, "`") {
		t.Errorf("postgres でバッククォートは使えない: %q", d.CreateMigrations)
	}
	if got := d.CountMigration("001.sql"); !strings.Contains(got, "sashiki_meta._migrations") {
		t.Errorf("CountMigration = %q", got)
	}
	if got := d.InsertMigration("001.sql"); !strings.Contains(got, "INSERT INTO sashiki_meta._migrations") {
		t.Errorf("InsertMigration = %q", got)
	}
}

func TestMySQLDialectUnchanged(t *testing.T) {
	d := MySQLDialect()
	if !strings.Contains(d.CreateMeta, "CREATE DATABASE IF NOT EXISTS") {
		t.Errorf("mysql は専用 DB を作るべき: %q", d.CreateMeta)
	}
	if !strings.Contains(d.CreateMigrations, "`sashiki_meta`._migrations") {
		t.Errorf("mysql はバッククォート識別子: %q", d.CreateMigrations)
	}
}

// ApplyDir は Ops.Dialect を使い、未設定なら MySQL 方言に倒すこと
// (手組み Ops の既存呼び出し・テストを壊さないため)。
func TestApplyDirUsesDialectAndDefaultsToMySQL(t *testing.T) {
	dir := t.TempDir()
	writeSQL(t, dir, "001_init.sql", "SELECT 1;")

	for _, tc := range []struct {
		name    string
		dialect Dialect
		want    string
	}{
		{"未設定なら mysql", Dialect{}, "CREATE DATABASE IF NOT EXISTS"},
		{"postgres を指定", PostgresDialect(), "CREATE SCHEMA IF NOT EXISTS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queries []string
			ops := Ops{
				Dialect: tc.dialect,
				Start:   func(context.Context, Server) error { return nil },
				WaitReady: func(context.Context, Server, time.Duration) error {
					return nil
				},
				Query: func(_ context.Context, _ Server, sql string) (string, error) {
					queries = append(queries, sql)
					if strings.HasPrefix(sql, "SELECT COUNT(*)") {
						return "0", nil
					}
					return "", nil
				},
				ApplyFile: func(context.Context, Server, string, string) error { return nil },
				Shutdown:  func(context.Context, Server) error { return nil },
				WaitGone: func(context.Context, Server, time.Duration) error {
					return nil
				},
			}
			applied, err := ApplyDir(context.Background(), Server{}, ops, dir, "app")
			if err != nil {
				t.Fatal(err)
			}
			if len(applied) != 1 {
				t.Fatalf("applied = %v, want 1 file", applied)
			}
			if len(queries) == 0 || !strings.Contains(queries[0], tc.want) {
				t.Errorf("最初のクエリ = %q, want prefix %q", queries, tc.want)
			}
		})
	}
}

func writeSQL(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
