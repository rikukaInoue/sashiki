// twig baseline: ベースライン管理。
// import はローカル root 操作(twigd 不要): base の mysqld を初期化 →
// ダンプ投入 → 接続ユーザー作成 → 正常終了 → @baseline snapshot。
// 「スナップショットは必ず正常終了状態で撮る」不変条件はここで守られる。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rikukaInoue/twig/internal/config"
)

func cmdBaseline(args []string) int {
	if len(args) < 1 {
		return usageBaseline()
	}
	switch args[0] {
	case "import":
		return cmdBaselineImport(args[1:])
	case "list":
		return cmdBaselineList(args[1:])
	default:
		return usageBaseline()
	}
}

func usageBaseline() int {
	fmt.Fprint(os.Stderr, `Usage:
  twig baseline import --from <dump.sql> [--config <path>]   ベース構築 + @baseline 取得 (root)
  twig baseline list [--json]                                snapshot 一覧 (twigd 経由)
`)
	return exitUsage
}

// --- import ---

type baselineImportOpts struct {
	from       string
	configPath string
}

func cmdBaselineImport(args []string) int {
	opts := baselineImportOpts{configPath: "/etc/twig/config.yaml"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--from":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.from = args[i]
		case "--config":
			i++
			if i >= len(args) {
				return usageBaseline()
			}
			opts.configPath = args[i]
		default:
			return usageBaseline()
		}
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "twig baseline import: root で実行してください")
		return exitError
	}
	if opts.from != "" {
		if _, err := os.Stat(opts.from); err != nil {
			fmt.Fprintf(os.Stderr, "twig baseline import: --from %s: %v\n", opts.from, err)
			return exitError
		}
	}
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "twig baseline import: config: %v\n", err)
		return exitError
	}
	if err := runBaselineImport(cfg, opts); err != nil {
		fmt.Fprintln(os.Stderr, "twig baseline import:", err)
		return exitError
	}
	return exitOK
}

func runBaselineImport(cfg config.Config, opts baselineImportOpts) error {
	base := cfg.Storage.Zfs.BaseDataset
	snap := base + "@" + cfg.Storage.Zfs.BaselineSnapshot

	// 既存 @baseline があれば失敗(上書きは派生ブランチを壊すため refresh フローで扱う)
	if err := exec.Command("zfs", "list", snap).Run(); err == nil {
		return fmt.Errorf("snapshot %s は既に存在します。撮り直しは baseline refresh (issue #10) で対応予定", snap)
	}

	mountOut, err := exec.Command("zfs", "get", "-H", "-o", "value", "mountpoint", base).Output()
	if err != nil {
		return fmt.Errorf("dataset %s が見つかりません。先に twig init を実行してください", base)
	}
	dataDir := filepath.Join(strings.TrimSpace(string(mountOut)), "data")

	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s が空ではありません。初期化済みの base に import はできません", dataDir)
	}

	mysqlUID, mysqlGID, err := lookupMysqlUser()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	if err := chownR(filepath.Dir(dataDir), mysqlUID, mysqlGID); err != nil {
		return err
	}

	sock := "/tmp/twig-baseline.sock"
	logErr := filepath.Join(cfg.Hooks.LogDir, "..", "baseline.err")

	fmt.Println("→ mysqld 初期化")
	if err := runAsUser(mysqlUID, mysqlGID, "/usr/sbin/mysqld", "--initialize-insecure", "--datadir="+dataDir); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	fmt.Println("→ mysqld 起動")
	if err := runAsUser(mysqlUID, mysqlGID, "/usr/sbin/mysqld",
		"--datadir="+dataDir, "--port=0", "--skip-networking",
		"--socket="+sock, "--pid-file=/tmp/twig-baseline.pid",
		"--log-error="+logErr, "--daemonize"); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = exec.Command("mysqladmin", "-uroot", "-S", sock, "shutdown").Run()
		}
	}()
	if err := waitSocket(sock, 60*time.Second); err != nil {
		return err
	}

	if opts.from != "" {
		fmt.Printf("→ ダンプ投入 (%s)\n", opts.from)
		dump, err := os.Open(opts.from)
		if err != nil {
			return err
		}
		defer func() { _ = dump.Close() }()
		load := exec.Command("mysql", "-uroot", "-S", sock)
		load.Stdin = dump
		if out, err := load.CombinedOutput(); err != nil {
			return fmt.Errorf("load dump: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	fmt.Printf("→ 接続ユーザー %s 作成\n", cfg.Engine.Mysql.ProxyUser)
	createUser := fmt.Sprintf(
		"CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'; GRANT ALL PRIVILEGES ON *.* TO '%s'@'%%'; FLUSH PRIVILEGES;",
		cfg.Engine.Mysql.ProxyUser, cfg.Engine.Mysql.ProxyPass, cfg.Engine.Mysql.ProxyUser)
	userCmd := exec.Command("mysql", "-uroot", "-S", sock, "-e", createUser)
	if out, err := userCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create user: %w: %s", err, strings.TrimSpace(string(out)))
	}

	fmt.Println("→ 正常終了")
	if out, err := exec.Command("mysqladmin", "-uroot", "-S", sock, "shutdown").CombinedOutput(); err != nil {
		return fmt.Errorf("shutdown: %w: %s", err, strings.TrimSpace(string(out)))
	}
	stopped = true
	if err := waitGone("/tmp/twig-baseline.pid", 30*time.Second); err != nil {
		return err
	}

	fmt.Printf("→ snapshot %s 取得\n", snap)
	if out, err := exec.Command("zfs", "snapshot", snap).CombinedOutput(); err != nil {
		return fmt.Errorf("snapshot: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("baseline import 完了。twig create <name> でブランチを作れます")
	return nil
}

func lookupMysqlUser() (uint32, uint32, error) {
	u, err := user.Lookup("mysql")
	if err != nil {
		return 0, 0, fmt.Errorf("mysql ユーザーが見つかりません(mysql-server はインストール済み?): %w", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uint32(uid), uint32(gid), nil
}

func runAsUser(uid, gid uint32, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func waitSocket(sock string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if exec.Command("mysqladmin", "-uroot", "-S", sock, "ping").Run() == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("mysqld が %s 以内に ready になりませんでした", timeout)
}

func waitGone(pidFile string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); os.IsNotExist(err) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("mysqld の停止を確認できませんでした (%s が残っています)", pidFile)
}

func chownR(root string, uid, gid uint32) error {
	return filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, int(uid), int(gid))
	})
}

// --- list (twigd 経由) ---

func cmdBaselineList(args []string) int {
	_, _, jsonOut, err := parseFlags(args)
	if err != nil {
		return usageBaseline()
	}
	code, data, err := call("GET", "/v1/baseline", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twig:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "twig:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var resp struct {
		Current   string   `json:"current"`
		Snapshots []string `json:"snapshots"`
	}
	_ = json.Unmarshal(data, &resp)
	fmt.Println("current:", resp.Current)
	for _, s := range resp.Snapshots {
		marker := "  "
		if s == resp.Current {
			marker = "* "
		}
		fmt.Println(marker + s)
	}
	return exitOK
}
