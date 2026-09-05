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

func TestFsxZfsRequiresFields(t *testing.T) {
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
	// parent_volume_id は無くてもよい
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
