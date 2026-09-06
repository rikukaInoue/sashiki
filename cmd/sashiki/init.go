// sashiki init: ホストのセットアップを自動化する(仕様 12-5)。
// apt・AppArmor・zpool・データセット・systemd ユニット・config 生成を行う。
// 各ステップは冪等。
package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"
)

//go:embed assets/mysqld@.service
var mysqldUnit []byte

//go:embed assets/config.yaml.tmpl
var configTmpl string

type initOpts struct {
	pool         string
	device       string
	skipPackages bool
	yes          bool
}

// initStep は 1 ステップ。done が true を返したらスキップする。
type initStep struct {
	name string
	done func() bool
	run  func() error
}

func cmdInit(args []string) int {
	opts := initOpts{pool: "dbpool"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pool":
			i++
			if i >= len(args) {
				return usage()
			}
			opts.pool = args[i]
		case "--device":
			i++
			if i >= len(args) {
				return usage()
			}
			opts.device = args[i]
		case "--skip-packages":
			opts.skipPackages = true
		case "--yes", "-y":
			opts.yes = true
		default:
			return usage()
		}
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "sashiki init: root で実行してください (sudo sashiki init ...)")
		return exitError
	}

	steps := initSteps(opts)
	fmt.Printf("sashiki init: pool=%s device=%s\n", opts.pool, orDash(opts.device))
	if !opts.yes {
		fmt.Print("続行する? [y/N]: ")
		var ans string
		_, _ = fmt.Scanln(&ans)
		if !strings.HasPrefix(strings.ToLower(ans), "y") {
			fmt.Println("中止しました")
			return exitError
		}
	}
	for _, s := range steps {
		if s.done != nil && s.done() {
			fmt.Printf("  ✓ %s (済み・スキップ)\n", s.name)
			continue
		}
		fmt.Printf("  → %s\n", s.name)
		if err := s.run(); err != nil {
			fmt.Fprintf(os.Stderr, "sashiki init: %s: %v\n", s.name, err)
			return exitError
		}
	}
	fmt.Println(`
init 完了。次のステップ:
  1. ベースデータを投入して baseline の snapshot を取得する:
       mysqld を ` + "`/" + opts.pool + "/base/data`" + ` で初期化・起動 → データ投入 → 正常終了 →
       zfs snapshot ` + opts.pool + `/base@baseline
     (examples/ の baseline スクリプト参照)
  2. sashikid を起動: systemctl enable --now sashikid
  3. ブランチを作る: sashiki create pr-1`)
	return exitOK
}

func initSteps(opts initOpts) []initStep {
	steps := []initStep{}

	if !opts.skipPackages {
		steps = append(steps, initStep{
			name: "パッケージ導入 (zfsutils-linux, mysql-server-8.0, mysql-client-8.0)",
			done: func() bool { return cmdOK("zfs", "version") && binExists("/usr/sbin/mysqld") },
			run: func() error {
				return runCmd(envv("DEBIAN_FRONTEND=noninteractive"), "apt-get", "install", "-y", "-q",
					"zfsutils-linux", "mysql-server-8.0", "mysql-client-8.0")
			},
		})
	}

	steps = append(steps,
		initStep{
			name: "既定の mysql サービスを停止・無効化",
			done: func() bool { return !cmdOK("systemctl", "is-enabled", "--quiet", "mysql") },
			run: func() error {
				_ = runCmd(nil, "systemctl", "stop", "mysql")
				return runCmd(nil, "systemctl", "disable", "mysql")
			},
		},
		initStep{
			name: "AppArmor: mysqld を datadir に閉じ込める完全プロファイルを生成(enforce)",
			done: func() bool {
				// 内容が最新の生成結果と一致する場合のみスキップ(pool 変更や
				// プロファイル更新時は再生成・再ロードする)
				return fileEqual(apparmorProfilePath, []byte(apparmorProfile(opts.pool)))
			},
			run: func() error {
				// Ubuntu 24.04 の /etc/apparmor.d/usr.sbin.mysqld は空の
				// プレースホルダで local override は no-op のため、sashiki 自前の
				// 完全プロファイルを配布して enforce でロードする(#79)。
				return installApparmorProfile(opts.pool)
			},
		},
		initStep{
			name: "sudoers: sashiki ユーザーを zfs/systemctl の限定操作に制限",
			done: func() bool {
				// 内容が最新の生成結果と一致する場合のみスキップ(旧形式の
				// 緩い sudoers は上書きして絞り直す #78)
				return fileEqual(sudoersPath, []byte(sudoersContent(opts.pool)))
			},
			run: func() error {
				// /usr/sbin/zfs 全体は広すぎる。実呼び出し形に合わせてパス制限し、
				// visudo -cf 検証後に本置きする(#78)。root-helper 化は v0.3。
				return installSudoers(opts.pool)
			},
		},
		initStep{
			name: fmt.Sprintf("zpool %s", opts.pool),
			done: func() bool { return cmdOK("zpool", "list", opts.pool) },
			run: func() error {
				if opts.device == "" {
					return fmt.Errorf("pool %q が存在しません。--device <dev> を指定してください (lsblk で確認)", opts.pool)
				}
				if err := runCmd(nil, "zpool", "create", "-o", "ashift=12", opts.pool, opts.device); err != nil {
					return err
				}
				return runCmd(nil, "zfs", "set", "compression=lz4", "atime=off", opts.pool)
			},
		},
		initStep{
			name: fmt.Sprintf("データセット %s/base (recordsize=16k)", opts.pool),
			done: func() bool { return cmdOK("zfs", "list", opts.pool+"/base") },
			run: func() error {
				return runCmd(nil, "zfs", "create", "-o", "recordsize=16k", "-o", "logbias=throughput", opts.pool+"/base")
			},
		},
		initStep{
			name: fmt.Sprintf("データセット %s/branches", opts.pool),
			done: func() bool { return cmdOK("zfs", "list", opts.pool+"/branches") },
			run:  func() error { return runCmd(nil, "zfs", "create", opts.pool+"/branches") },
		},
		initStep{
			name: "ディレクトリ作成 (/etc/sashiki, /var/lib/sashiki, /var/log/sashiki)",
			run: func() error {
				for _, d := range []string{"/etc/sashiki/hooks", "/var/lib/sashiki/branches", "/var/log/sashiki/hooks"} {
					if err := os.MkdirAll(d, 0o755); err != nil {
						return err
					}
				}
				// mysqld がエラーログを書けるように
				return runCmd(nil, "chown", "-R", "mysql:mysql", "/var/log/sashiki")
			},
		},
		initStep{
			name: "systemd ユニット mysqld@.service",
			done: func() bool { return fileEqual("/etc/systemd/system/mysqld@.service", mysqldUnit) },
			run: func() error {
				if err := os.WriteFile("/etc/systemd/system/mysqld@.service", mysqldUnit, 0o644); err != nil {
					return err
				}
				return runCmd(nil, "systemctl", "daemon-reload")
			},
		},
		initStep{
			name: "/etc/sashiki/config.yaml 生成",
			done: func() bool { _, err := os.Stat("/etc/sashiki/config.yaml"); return err == nil },
			run: func() error {
				cfg, err := renderConfig(opts.pool)
				if err != nil {
					return err
				}
				return os.WriteFile("/etc/sashiki/config.yaml", cfg, 0o644)
			},
		},
	)
	return steps
}

func renderConfig(pool string) ([]byte, error) {
	t, err := template.New("config").Parse(configTmpl)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, struct{ Pool string }{Pool: pool}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- helpers ---

func cmdOK(name string, args ...string) bool {
	return exec.Command(name, args...).Run() == nil
}

func binExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&0o111 != 0
}

func fileEqual(path string, want []byte) bool {
	got, err := os.ReadFile(path)
	return err == nil && bytes.Equal(got, want)
}

func runCmd(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func envv(kv ...string) []string { return kv }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
