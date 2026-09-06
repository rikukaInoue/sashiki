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
