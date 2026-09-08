package config

import (
	"os"
	"path/filepath"
	"testing"
)

func load(t *testing.T, yaml string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBackendNewNames(t *testing.T) {
	cfg := load(t, `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: mypool
    base_dataset: mypool/base
    branch_parent: mypool/branches
    baseline_snapshot: baseline
`)
	if cfg.Storage.Backend != "ebs-zfs" || cfg.Storage.Zfs.Pool != "mypool" {
		t.Errorf("cfg = %+v", cfg.Storage)
	}
}

func TestBackendLegacyNamesNormalized(t *testing.T) {
	cfg := load(t, `
storage:
  backend: zfs
  zfs:
    pool: oldpool
`)
	if cfg.Storage.Backend != "ebs-zfs" {
		t.Errorf("backend = %q, want ebs-zfs (normalized)", cfg.Storage.Backend)
	}
	if cfg.Storage.Zfs.Pool != "oldpool" {
		t.Errorf("legacy zfs key should be carried over: %+v", cfg.Storage.Zfs)
	}
}

func TestFsxZfsRequiresFieldsButNotParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte(`
storage:
  backend: fsx-zfs
  fsx-zfs:
    region: ap-northeast-1
`), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("missing fsx-zfs fields should fail validation")
	}
	// parent_volume_id は無くてもよい(自動発見)
	_ = os.WriteFile(path, []byte(`
storage:
  backend: fsx-zfs
  fsx-zfs:
    region: ap-northeast-1
    filesystem_id: fs-1
    base_volume_id: fsvol-1
    dns_name: fs-1.fsx.test
`), 0o644)
	if _, err := Load(path); err != nil {
		t.Errorf("parent_volume_id should be optional: %v", err)
	}
}

func TestDefaultProfilesSeeded(t *testing.T) {
	cfg := Default()
	if cfg.Branches.DefaultProfile != "preview" {
		t.Errorf("default_profile = %q, want preview", cfg.Branches.DefaultProfile)
	}
	for _, name := range []string{"preview", "ci", "sandbox"} {
		if _, ok := cfg.Branches.Profiles[name]; !ok {
			t.Errorf("profile %q not seeded", name)
		}
	}
	if cfg.Branches.Profiles["ci"].IdleStopAfter == 0 {
		t.Error("ci profile idle_stop_after should be set")
	}
}

func TestAppUserAliasesProxyUser(t *testing.T) {
	cfg := load(t, `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
engine:
  mysql:
    app_user: appdev
    app_pass: s3cret
`)
	if cfg.Engine.Mysql.ProxyUser != "appdev" {
		t.Errorf("proxy_user = %q, want appdev (aliased from app_user)", cfg.Engine.Mysql.ProxyUser)
	}
	if cfg.Engine.Mysql.ProxyPass != "s3cret" {
		t.Errorf("proxy_pass = %q, want s3cret (aliased from app_pass)", cfg.Engine.Mysql.ProxyPass)
	}
}

func TestProxyUserStillWorksWithoutAppUser(t *testing.T) {
	cfg := load(t, `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
engine:
  mysql:
    proxy_user: legacy
`)
	if cfg.Engine.Mysql.ProxyUser != "legacy" {
		t.Errorf("proxy_user = %q, want legacy", cfg.Engine.Mysql.ProxyUser)
	}
}

func TestRunLogDirsDerived(t *testing.T) {
	// 未設定 → 既定と派生値。
	cfg := load(t, `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
`)
	if cfg.RunDir != "/run/sashiki" || cfg.LogDir != "/var/log/sashiki" {
		t.Errorf("run_dir/log_dir defaults = %q / %q", cfg.RunDir, cfg.LogDir)
	}
	if cfg.Hooks.LogDir != "/var/log/sashiki/hooks" {
		t.Errorf("hooks.log_dir = %q, want derived from log_dir", cfg.Hooks.LogDir)
	}
	if cfg.Baseline.MaskedSentinel != "/run/sashiki/baseline-masked" {
		t.Errorf("masked_sentinel = %q, want derived from run_dir", cfg.Baseline.MaskedSentinel)
	}
	// run_dir / log_dir を上書き → 派生値も追従。
	cfg = load(t, `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
run_dir: /tmp/run
log_dir: /tmp/log
`)
	if cfg.Hooks.LogDir != "/tmp/log/hooks" {
		t.Errorf("hooks.log_dir = %q, want /tmp/log/hooks", cfg.Hooks.LogDir)
	}
	if cfg.Baseline.MaskedSentinel != "/tmp/run/baseline-masked" {
		t.Errorf("masked_sentinel = %q, want /tmp/run/baseline-masked", cfg.Baseline.MaskedSentinel)
	}
}

func TestExplicitHookLogDirWins(t *testing.T) {
	cfg := load(t, `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
hooks:
  log_dir: /custom/hooklogs
`)
	if cfg.Hooks.LogDir != "/custom/hooklogs" {
		t.Errorf("explicit hooks.log_dir should win, got %q", cfg.Hooks.LogDir)
	}
}

func TestLocalBackendRequiresRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	// apfs だが local.root が無い → Validate エラー
	_ = os.WriteFile(path, []byte(`
storage:
  backend: apfs
`), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("apfs without local.root should fail validation")
	}
	// root を与えれば通る
	_ = os.WriteFile(path, []byte(`
storage:
  backend: reflink
  local:
    root: /mnt/xfs/sashiki
`), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("reflink with root should load: %v", err)
	}
	if cfg.Storage.Local.Root != "/mnt/xfs/sashiki" {
		t.Errorf("local.root = %q", cfg.Storage.Local.Root)
	}
}

func TestEngineProcessMode(t *testing.T) {
	cfg := load(t, `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
engine:
  mysql:
    mode: process
    mysqld_bin: /opt/homebrew/opt/mysql/bin/mysqld
`)
	if cfg.Engine.Mysql.Mode != "process" {
		t.Errorf("mode = %q, want process", cfg.Engine.Mysql.Mode)
	}
	if cfg.Engine.Mysql.MysqldBin != "/opt/homebrew/opt/mysql/bin/mysqld" {
		t.Errorf("mysqld_bin = %q", cfg.Engine.Mysql.MysqldBin)
	}
}

func TestDefaultProfileMustExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	// profiles を上書きしつつ default_profile が存在しない → Validate エラー
	yaml := `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
branches:
  default_profile: nonexistent
  profiles:
    ci: { idle_stop_after: 5m }
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load should fail when default_profile is not in profiles")
	}
}

func TestProxyAllowedUser(t *testing.T) {
	base := "storage:\n  backend: ebs-zfs\n  ebs-zfs:\n    pool: p\n"
	// 未設定 → nil(既定 = app_user のみ)
	cfg := load(t, base)
	if cfg.Proxy.AllowedUser != nil {
		t.Errorf("unset allowed_user should be nil, got %q", *cfg.Proxy.AllowedUser)
	}
	// 空文字 → 非 nil の ""(任意ユーザー許可)
	cfg = load(t, base+"proxy:\n  allowed_user: \"\"\n")
	if cfg.Proxy.AllowedUser == nil || *cfg.Proxy.AllowedUser != "" {
		t.Errorf(`allowed_user: "" should be non-nil empty, got %v`, cfg.Proxy.AllowedUser)
	}
	// 特定名
	cfg = load(t, base+"proxy:\n  allowed_user: admin\n")
	if cfg.Proxy.AllowedUser == nil || *cfg.Proxy.AllowedUser != "admin" {
		t.Errorf("allowed_user: admin, got %v", cfg.Proxy.AllowedUser)
	}
}

// engine 非依存アクセサ(#225): postgres 選択時に postgres 側の設定を返し、
// mysql 選択時は従来どおり mysql 側を返すこと。
func TestEngineAccessorsPostgres(t *testing.T) {
	cfg := load(t, `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
    base_dataset: p/base
    branch_parent: p/branches
    baseline_snapshot: baseline
engine:
  type: postgres
  postgres:
    app_user: appuser
    app_pass: apppass
    mode: process
    run_user: pg
    shared_buffers: 512M
    expected_rss: 700M
    memory_headroom: 1G
    max_running: 7
    env_dir: /etc/sashiki-pg
`)
	if got := cfg.AppUser(); got != "appuser" {
		t.Errorf("AppUser = %q, want appuser", got)
	}
	if got := cfg.AppPass(); got != "apppass" {
		t.Errorf("AppPass = %q, want apppass", got)
	}
	if got := cfg.EngineMode(); got != "process" {
		t.Errorf("EngineMode = %q, want process", got)
	}
	if got := cfg.EngineRunUser(); got != "pg" {
		t.Errorf("EngineRunUser = %q, want pg", got)
	}
	if got := cfg.MemoryBaselineSize(); got != "512M" {
		t.Errorf("MemoryBaselineSize = %q, want 512M", got)
	}
	if got := cfg.ExpectedRSS(); got != "700M" {
		t.Errorf("ExpectedRSS = %q, want 700M", got)
	}
	if got := cfg.MemoryHeadroom(); got != "1G" {
		t.Errorf("MemoryHeadroom = %q, want 1G", got)
	}
	if got := cfg.MaxRunning(); got != 7 {
		t.Errorf("MaxRunning = %d, want 7", got)
	}
	if got := cfg.EngineEnvDir(); got != "/etc/sashiki-pg" {
		t.Errorf("EngineEnvDir = %q, want /etc/sashiki-pg", got)
	}
	if got := cfg.PortRange(); got[0] != 5433 {
		t.Errorf("PortRange = %v, want postgres default low 5433", got)
	}
}

func TestEngineAccessorsMysqlUnchanged(t *testing.T) {
	cfg := load(t, `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
    base_dataset: p/base
    branch_parent: p/branches
    baseline_snapshot: baseline
engine:
  type: mysql
  mysql:
    app_user: mysqluser
    app_pass: mysqlpass
    buffer_pool_size: 256M
    max_running: 3
`)
	if got := cfg.AppUser(); got != "mysqluser" {
		t.Errorf("AppUser = %q, want mysqluser", got)
	}
	if got := cfg.AppPass(); got != "mysqlpass" {
		t.Errorf("AppPass = %q, want mysqlpass", got)
	}
	if got := cfg.MemoryBaselineSize(); got != "256M" {
		t.Errorf("MemoryBaselineSize = %q, want 256M", got)
	}
	if got := cfg.MaxRunning(); got != 3 {
		t.Errorf("MaxRunning = %d, want 3", got)
	}
	// mode 未設定は systemd 扱い
	if got := cfg.EngineMode(); got != "systemd" {
		t.Errorf("EngineMode = %q, want systemd", got)
	}
}

// postgres 既定値: app_user/app_pass と shared_buffers が入っていること。
func TestPostgresDefaults(t *testing.T) {
	cfg := load(t, `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
    base_dataset: p/base
    branch_parent: p/branches
    baseline_snapshot: baseline
engine:
  type: postgres
`)
	if cfg.AppUser() != "dev" || cfg.AppPass() != "dev" {
		t.Errorf("postgres default app credential = %q/%q, want dev/dev", cfg.AppUser(), cfg.AppPass())
	}
	if cfg.MemoryBaselineSize() != "128M" {
		t.Errorf("postgres default shared_buffers = %q, want 128M", cfg.MemoryBaselineSize())
	}
	if cfg.EngineRunUser() != "postgres" {
		t.Errorf("postgres default run_user = %q, want postgres", cfg.EngineRunUser())
	}
}

func TestInvalidEngineMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: p
    base_dataset: p/base
    branch_parent: p/branches
    baseline_snapshot: baseline
engine:
  type: postgres
  postgres:
    mode: bogus
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("engine.postgres.mode: bogus should be rejected")
	}
}
