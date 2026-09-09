package baseline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PostgreSQL 版の組み込みローダー(#226)。MySQL 版(RealOps)と同じ Ops の形で
// pg_ctl / psql を叩く。ApplyDir 側は方言(PostgresDialect)で吸収するので、
// 冪等化のロジックは MySQL と共通。
//
// 一時クラスタは TCP を開かず(listen_addresses='')unix socket だけで受ける。
// 稼働中のブランチとポートが衝突しないため。

// PgSocketDir / PgPort は Server の Socket / PgPort が未設定のときの既定。
const (
	pgDefaultSocketDir = "/tmp"
	pgDefaultPort      = 5498
)

// pgSocketDir は接続に使う socket ディレクトリを返す。
// Server.Socket は MySQL では socket ファイルだが、PostgreSQL では
// ディレクトリとして扱う(psql -h <dir> の形)。
func (s Server) pgSocketDir() string {
	if s.Socket == "" {
		return pgDefaultSocketDir
	}
	// ファイルパスを渡された場合はその親を使う(呼び出し側の取り違え対策)。
	if strings.HasSuffix(s.Socket, ".sock") {
		return filepath.Dir(s.Socket)
	}
	return s.Socket
}

func (s Server) pgPort() int {
	if s.PgPort > 0 {
		return s.PgPort
	}
	return pgDefaultPort
}

// pgBin は bin_dir 配下のツールを返す(未設定なら PATH 解決)。
func (s Server) pgBin(name string) string {
	if s.PgBinDir != "" {
		p := filepath.Join(s.PgBinDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

// pgEnv は psql / pg_restore がこの一時クラスタへ繋ぐための環境変数。
func (s Server) pgEnv() []string {
	return []string{
		"PGHOST=" + s.pgSocketDir(),
		"PGPORT=" + strconv.Itoa(s.pgPort()),
		"PGCONNECT_TIMEOUT=10",
	}
}

// PostgresOps は実コマンドを実行する Ops(PostgreSQL)。
func PostgresOps() Ops {
	return Ops{
		Dialect: PostgresDialect(),
		// Initialize は refresh 経路では使わない(既存 base に適用するため)。
		Initialize: func(ctx context.Context, s Server) error {
			return fmt.Errorf("postgres の initialize は baseline import を使ってください")
		},
		Start: func(ctx context.Context, s Server) error {
			opts := fmt.Sprintf("-p %d -c listen_addresses='' -c unix_socket_directories=%s",
				s.pgPort(), s.pgSocketDir())
			// -l は必須。渡さないとデーモン化した postgres が親の stdout/stderr を
			// 握り続け、出力をパイプで捕まえる実行だと永久にブロックする(#223)。
			return runPgNoPipe(ctx, s, s.pgBin("pg_ctl"), "start", "-D", s.DataDir,
				"-o", opts, "-l", s.pgLogPath(), "-w", "-t", "60")
		},
		WaitReady: func(ctx context.Context, s Server, timeout time.Duration) error {
			deadline := time.Now().Add(timeout)
			for time.Now().Before(deadline) {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := runPg(ctx, s, s.pgBin("pg_isready"),
					"-h", s.pgSocketDir(), "-p", strconv.Itoa(s.pgPort())); err == nil {
					return nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			return fmt.Errorf("timeout: postgres が %s 以内に ready になりませんでした", timeout)
		},
		Query: func(ctx context.Context, s Server, sql string) (string, error) {
			out, err := outputPg(ctx, s, s.pgBin("psql"),
				"-w", "-v", "ON_ERROR_STOP=1", "-tAc", sql, "-d", pgDBOr(s, ""))
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(out), nil
		},
		ApplyFile: func(ctx context.Context, s Server, db, path string) error {
			// ON_ERROR_STOP が無いと途中のエラーを黙って飛ばして「成功」してしまう。
			return runPg(ctx, s, s.pgBin("psql"),
				"-w", "-v", "ON_ERROR_STOP=1", "-d", pgDBOr(s, db), "-f", path)
		},
		Shutdown: func(ctx context.Context, s Server) error {
			// fast(正常終了)。snapshot の一貫性がこれに依存する。
			return runPg(ctx, s, s.pgBin("pg_ctl"), "stop", "-D", s.DataDir, "-m", "fast", "-w", "-t", "60")
		},
		WaitGone: func(ctx context.Context, s Server, timeout time.Duration) error {
			deadline := time.Now().Add(timeout)
			pid := filepath.Join(s.DataDir, "postmaster.pid")
			for time.Now().Before(deadline) {
				if _, err := os.Stat(pid); os.IsNotExist(err) {
					return nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			return fmt.Errorf("timeout: postgres が %s 以内に停止しませんでした", timeout)
		},
	}
}

// pgLogPath は一時クラスタのサーバログ。datadir の中に置くと snapshot に
// 入ってしまうので外に出す。
func (s Server) pgLogPath() string {
	if s.LogError != "" {
		return s.LogError
	}
	return filepath.Join(os.TempDir(), "sashiki-refresh-pg.log")
}

// pgDBOr は接続先 DB を決める。db 指定が無ければ source_db、それも無ければ
// postgres(必ず存在する既定 DB)。
func pgDBOr(s Server, db string) string {
	if db != "" {
		return db
	}
	if s.PgDB != "" {
		return s.PgDB
	}
	return "postgres"
}

func pgCmd(ctx context.Context, s Server, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), s.pgEnv()...)
	if s.UID != 0 || s.GID != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: s.UID, Gid: s.GID},
		}
		cmd.Env = append(cmd.Env, "HOME="+os.TempDir())
	}
	return cmd
}

func runPg(ctx context.Context, s Server, name string, args ...string) error {
	out, err := pgCmd(ctx, s, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func outputPg(ctx context.Context, s Server, name string, args ...string) (string, error) {
	out, err := pgCmd(ctx, s, name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// runPgNoPipe は出力をパイプで捕まえずに実行する(pg_ctl start 専用)。
func runPgNoPipe(ctx context.Context, s Server, name string, args ...string) error {
	cmd := pgCmd(ctx, s, name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w\n--- %s ---\n%s", name, err, s.pgLogPath(), tailFile(s.pgLogPath(), 20))
	}
	return nil
}

// tailFile はファイル末尾の n 行(エラー添付用)。
func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(ログを読めませんでした: " + err.Error() + ")"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
