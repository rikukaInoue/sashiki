// PostgreSQL の baseline import(#223)。MySQL 版(baseline.go の
// runBaselineImport)と同じ段取り —— 空の base データセットにクラスタを作り、
// ダンプを投入し、接続ロールを用意し、**正常終了してから** snapshot を取得する
// —— を postgres のツール(initdb / psql / pg_restore / pg_ctl)で行う。
//
// 衝突面を小さくするため baseline.go には手を入れず、ここに閉じている
// (baseline.go は MySQL 側の変更が頻繁なため)。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/rikukaInoue/sashiki/internal/config"
)

// baseline 構築用に一時起動するクラスタの設定。TCP を開かず(listen_addresses=”)
// unix socket だけで受けるので、稼働中のブランチとポートが衝突しない。
const (
	pgBaselinePort   = "5499" // socket 名 .s.PGSQL.5499 に使うだけで LISTEN しない
	pgBaselineSocket = "/tmp"
)

func runPostgresBaselineImport(cfg config.Config, opts baselineImportOpts) error {
	if cfg.Storage.Backend == "apfs" || cfg.Storage.Backend == "reflink" {
		return fmt.Errorf("postgres + %s はまだ未対応です(postgres の process モードが必要 / #227)。"+
			"いまは ebs-zfs / fsx-zfs で使ってください", cfg.Storage.Backend)
	}

	base := cfg.Storage.Zfs.BaseDataset
	snap := base + "@" + cfg.Storage.Zfs.BaselineSnapshot
	if err := exec.Command("zfs", "list", snap).Run(); err == nil {
		return fmt.Errorf("snapshot %s は既に存在します。取得し直しは baseline refresh で行ってください", snap)
	}

	mountOut, err := exec.Command("zfs", "get", "-H", "-o", "value", "mountpoint", base).Output()
	if err != nil {
		return fmt.Errorf("dataset %s が見つかりません。先に sashiki init を実行してください", base)
	}
	dataDir := filepath.Join(strings.TrimSpace(string(mountOut)), "data")
	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s が空ではありません。初期化済みの base に import はできません", dataDir)
	}

	runUser := cfg.EngineRunUser()
	if runUser == "" {
		runUser = "postgres"
	}
	uid, gid, err := lookupOSUser(runUser)
	if err != nil {
		return err
	}
	// postgres は datadir のパーミッションが 0700(または 0750)でないと起動を拒む。
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := chownR(filepath.Dir(dataDir), uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return err
	}

	binDir := cfg.Engine.Postgres.BinDir
	initdb := pgBin(binDir, "initdb")
	pgCtl := pgBin(binDir, "pg_ctl")
	psql := pgBin(binDir, "psql")
	pgRestore := pgBin(binDir, "pg_restore")

	// --- initdb ---
	// host 認証を scram-sha-256 にして、proxy → backend の TCP 接続でパスワードを
	// 検証させる(#222 の pgproxy は app_user のパスワードを持っている)。local は
	// trust にして、この import 中の psql をパスワード無しで通す。
	fmt.Println("→ initdb")
	initArgs := []string{"-D", dataDir, "--auth-host=scram-sha-256", "--auth-local=trust"}
	initArgs = append(initArgs, cfg.Engine.Postgres.InitdbArgs...)
	if err := runAsUser(uid, gid, initdb, initArgs...); err != nil {
		return fmt.Errorf("initdb: %w", err)
	}

	// --- 起動(TCP を開かない) ---
	fmt.Println("→ postgres 起動")
	startOpts := fmt.Sprintf("-p %s -c listen_addresses='' -c unix_socket_directories=%s",
		pgBaselinePort, pgBaselineSocket)
	if err := runAsUser(uid, gid, pgCtl, "start", "-D", dataDir, "-o", startOpts, "-w"); err != nil {
		return fmt.Errorf("pg_ctl start: %w", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = runAsUser(uid, gid, pgCtl, "stop", "-D", dataDir, "-m", "immediate", "-w")
		}
	}()

	// --- ダンプ投入 ---
	db := opts.db
	if db == "" {
		db = "postgres"
	}
	if opts.db != "" {
		fmt.Printf("→ データベース %s 作成\n", opts.db)
		if err := pgSQL(uid, gid, psql, "postgres",
			fmt.Sprintf("CREATE DATABASE %s", quoteIdent(opts.db))); err != nil {
			return fmt.Errorf("create database %s: %w", opts.db, err)
		}
	}
	if opts.from != "" {
		if err := pgLoadDump(uid, gid, psql, pgRestore, opts.from, db, opts.threads); err != nil {
			return err
		}
	}

	// --- 接続ロール ---
	appUser, appPass := cfg.AppUser(), cfg.AppPass()
	fmt.Printf("→ 接続ロール %s 作成\n", appUser)
	// パスワードは scram-sha-256 で保存される(PG14+ の既定 password_encryption)。
	// proxy は認証を終端したうえで、このロールとして backend に繋ぎ直す。
	roleSQL := fmt.Sprintf("CREATE ROLE %s LOGIN SUPERUSER PASSWORD %s",
		quoteIdent(appUser), quoteLiteral(appPass))
	if err := pgSQL(uid, gid, psql, "postgres", roleSQL); err != nil {
		return fmt.Errorf("create role %s: %w", appUser, err)
	}
	if opts.db != "" {
		if err := pgSQL(uid, gid, psql, "postgres",
			fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", quoteIdent(opts.db), quoteIdent(appUser))); err != nil {
			return fmt.Errorf("alter database owner: %w", err)
		}
	}

	// --- 正常終了 → snapshot ---
	// snapshot は必ず正常終了状態でのみ取得する。ここで落ちるとブランチ起動時に
	// crash recovery が走り、@baseline が dirty な状態で固定されてしまう。
	fmt.Println("→ 正常終了")
	if err := runAsUser(uid, gid, pgCtl, "stop", "-D", dataDir, "-m", "fast", "-w"); err != nil {
		return fmt.Errorf("pg_ctl stop: %w", err)
	}
	stopped = true

	fmt.Printf("→ snapshot %s 取得\n", snap)
	if out, err := exec.Command("zfs", "snapshot", snap).CombinedOutput(); err != nil {
		return fmt.Errorf("snapshot: %w: %s", err, strings.TrimSpace(string(out)))
	}
	registerImportedBaseline(cfg.StateDB, snap, cfg.Storage.Zfs.BaselineSnapshot)
	fmt.Println("baseline import 完了。sashiki create <name> でブランチを作れます")
	return nil
}

// pgLoadDump は --from を投入する。プレーン SQL は psql、pg_dump のカスタム/
// ディレクトリ形式は pg_restore で流す(形式は中身を見て判定する)。
func pgLoadDump(uid, gid uint32, psql, pgRestore, from, db string, threads int) error {
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		// pg_dump -Fd(ディレクトリ形式)。-j で並列復元できる。
		fmt.Printf("→ ダンプ投入 (%s, ディレクトリ形式, %d 並列)\n", from, threads)
		args := []string{"-d", db, "--no-owner", "-j", strconv.Itoa(max(threads, 1)), from}
		return runAsUserWithEnv(uid, gid, pgEnv(), pgRestore, args...)
	}
	if isPgCustomDump(from) {
		fmt.Printf("→ ダンプ投入 (%s, カスタム形式)\n", from)
		return runAsUserWithEnv(uid, gid, pgEnv(), pgRestore, "-d", db, "--no-owner", from)
	}
	fmt.Printf("→ ダンプ投入 (%s, プレーン SQL)\n", from)
	// ON_ERROR_STOP が無いと途中のエラーを黙って飛ばして「成功」してしまう。
	return runAsUserWithEnv(uid, gid, pgEnv(), psql,
		"-v", "ON_ERROR_STOP=1", "-d", db, "-f", from)
}

// isPgCustomDump は pg_dump のカスタム形式(先頭が "PGDMP")かを判定する。
func isPgCustomDump(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 5)
	if _, err := f.Read(head); err != nil {
		return false
	}
	return string(head) == "PGDMP"
}

// pgSQL は一時クラスタへ SQL を 1 本流す(local trust なのでパスワード不要)。
func pgSQL(uid, gid uint32, psql, db, sql string) error {
	return runAsUserWithEnv(uid, gid, pgEnv(), psql,
		"-v", "ON_ERROR_STOP=1", "-d", db, "-c", sql)
}

// pgEnv は一時クラスタへ unix socket 経由で繋ぐための環境変数。
func pgEnv() []string {
	return []string{"PGHOST=" + pgBaselineSocket, "PGPORT=" + pgBaselinePort}
}

// pgBin は bin_dir 配下のツールを返す。bin_dir 未設定なら PATH に任せる。
func pgBin(binDir, name string) string {
	if binDir != "" {
		if p := filepath.Join(binDir, name); binExists(p) {
			return p
		}
	}
	return name
}

// lookupOSUser は OS ユーザーの uid/gid を引く。
func lookupOSUser(name string) (uint32, uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("%s ユーザーが見つかりません(postgresql はインストール済み?): %w", name, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uint32(uid), uint32(gid), nil
}

// quoteIdent / quoteLiteral は SQL の識別子 / 文字列リテラルを安全に埋め込む。
// 設定由来の値(ロール名・DB 名・パスワード)をそのまま連結しないため。
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// runAsUserWithEnv は runAsUser に環境変数を足したもの(postgres のツールは
// PGHOST / PGPORT で接続先を受け取る)。エラー時に出力を添えるのも同じ。
func runAsUserWithEnv(uid, gid uint32, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
