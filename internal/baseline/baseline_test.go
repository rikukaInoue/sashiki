package baseline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mockOps は呼び出し履歴を記録する Ops。applied には _migrations 登録済み扱いの
// ファイル名を入れる。
type mockOps struct {
	started, shutdown, waitGone bool
	appliedFiles                []string
	queries                     []string
	preApplied                  map[string]bool
	applyErr                    error
	shutdownErr                 error
}

func (m *mockOps) ops() Ops {
	return Ops{
		Start: func(ctx context.Context, s Server) error { m.started = true; return nil },
		WaitReady: func(ctx context.Context, s Server, d time.Duration) error {
			return nil
		},
		Query: func(ctx context.Context, s Server, sql string) (string, error) {
			m.queries = append(m.queries, sql)
			for name := range m.preApplied {
				if len(sql) > 0 && containsName(sql, name) {
					return "1", nil
				}
			}
			return "0", nil
		},
		ApplyFile: func(ctx context.Context, s Server, db, path string) error {
			if m.applyErr != nil {
				return m.applyErr
			}
			m.appliedFiles = append(m.appliedFiles, filepath.Base(path))
			return nil
		},
		Shutdown: func(ctx context.Context, s Server) error { m.shutdown = true; return m.shutdownErr },
		WaitGone: func(ctx context.Context, s Server, d time.Duration) error {
			m.waitGone = true
			return nil
		},
	}
}

func containsName(sql, name string) bool {
	return len(name) > 0 && len(sql) >= len(name) &&
		(func() bool { return stringContains(sql, "'"+name+"'") && stringContains(sql, "SELECT") })()
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func writeFiles(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("SELECT 1;"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestApplyDirAppliesInNameOrder(t *testing.T) {
	m := &mockOps{}
	dir := writeFiles(t, "0002_b.sql", "0001_a.sql", "0010_c.sql")
	applied, err := ApplyDir(context.Background(), Server{}, m.ops(), dir, "todo")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0001_a.sql", "0002_b.sql", "0010_c.sql"}
	if len(applied) != 3 || applied[0] != want[0] || applied[1] != want[1] || applied[2] != want[2] {
		t.Errorf("applied = %v, want %v", applied, want)
	}
	if !m.shutdown || !m.waitGone {
		t.Error("mysqld should be gracefully shut down and confirmed gone")
	}
}

func TestApplyDirSkipsRecorded(t *testing.T) {
	m := &mockOps{preApplied: map[string]bool{"0001_a.sql": true}}
	dir := writeFiles(t, "0001_a.sql", "0002_b.sql")
	applied, err := ApplyDir(context.Background(), Server{}, m.ops(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != "0002_b.sql" {
		t.Errorf("applied = %v, want [0002_b.sql]", applied)
	}
}

func TestApplyDirEmptyIsNoop(t *testing.T) {
	m := &mockOps{}
	applied, err := ApplyDir(context.Background(), Server{}, m.ops(), t.TempDir(), "")
	if err != nil || applied != nil {
		t.Errorf("empty dir should be a no-op, got %v / %v", applied, err)
	}
	if m.started {
		t.Error("mysqld should not be started for an empty dir")
	}
}

func TestApplyDirShutsDownOnFailure(t *testing.T) {
	m := &mockOps{applyErr: errors.New("boom")}
	dir := writeFiles(t, "0001_a.sql")
	if _, err := ApplyDir(context.Background(), Server{}, m.ops(), dir, ""); err == nil {
		t.Fatal("apply failure should propagate")
	}
	if !m.shutdown {
		t.Error("mysqld must be shut down even on failure (invariant)")
	}
}

func TestApplyDirShutdownFailureIsError(t *testing.T) {
	m := &mockOps{shutdownErr: errors.New("still running")}
	dir := writeFiles(t, "0001_a.sql")
	if _, err := ApplyDir(context.Background(), Server{}, m.ops(), dir, ""); err == nil {
		t.Fatal("shutdown failure must be an error (snapshot must not proceed)")
	}
}

func TestApplyDirRejectsBadNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad name'.sql"), []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyDir(context.Background(), Server{}, (&mockOps{}).ops(), dir, ""); err == nil {
		t.Fatal("invalid file name should be rejected")
	}
}

// #127: Server が mysqld/defaults/client を config から解決する。
func TestServerMysqldResolution(t *testing.T) {
	// 既定(未指定)
	s := Server{}
	if s.mysqld() != "/usr/sbin/mysqld" {
		t.Errorf("mysqld() default = %q", s.mysqld())
	}
	if s.defaultsArgs() != nil {
		t.Errorf("defaultsArgs() default should be nil, got %v", s.defaultsArgs())
	}
	if s.client("mysql") != "mysql" {
		t.Errorf("client default should be PATH name, got %q", s.client("mysql"))
	}
	// 明示指定
	s2 := Server{MysqldBin: "/opt/homebrew/opt/mysql@8.0/bin/mysqld", ExtraCnf: "/etc/server.cnf"}
	if s2.mysqld() != "/opt/homebrew/opt/mysql@8.0/bin/mysqld" {
		t.Errorf("mysqld() = %q", s2.mysqld())
	}
	da := s2.defaultsArgs()
	if len(da) != 1 || da[0] != "--defaults-file=/etc/server.cnf" {
		t.Errorf("defaultsArgs() = %v, want [--defaults-file=/etc/server.cnf]", da)
	}
}
