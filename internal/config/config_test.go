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
