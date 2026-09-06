// Package config は /etc/sashiki/config.yaml の読み込み(仕様 12-1 の v0.1 サブセット)。
// シークレットは設定ファイルに書かない。API トークンは環境変数から読む。
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config は sashikid 全体の設定。
type Config struct {
	Listen   Listen   `yaml:"listen"`
	Domain   string   `yaml:"domain"`
	StateDB  string   `yaml:"state_db"`
	Storage  Storage  `yaml:"storage"`
	Engine   Engine   `yaml:"engine"`
	Proxy    Proxy    `yaml:"proxy"`
	Branches Branches `yaml:"branches"`
	Baseline Baseline `yaml:"baseline"`
	Hooks    Hooks    `yaml:"hooks"`
	Auth     Auth     `yaml:"auth"`
}

// Listen は各リスナーのアドレス。
type Listen struct {
	API     string `yaml:"api"`
	Proxy   string `yaml:"proxy"`
	Metrics string `yaml:"metrics"`
}

// Storage はバックエンド設定。
type Storage struct {
	Backend string     `yaml:"backend"` // ebs-zfs | fsx-zfs
	Zfs     ZfsStorage `yaml:"ebs-zfs"`
	Fsx     FsxStorage `yaml:"fsx-zfs"`

	// 旧キー(v0.1 互換)。Load で新フィールドへ移す。
	LegacyZfs *ZfsStorage `yaml:"zfs"`
	LegacyFsx *FsxStorage `yaml:"fsx"`
}

// FsxStorage は fsx バックエンドの設定。
type FsxStorage struct {
	Region           string `yaml:"region"`
	FilesystemID     string `yaml:"filesystem_id"`
	BaseVolumeID     string `yaml:"base_volume_id"`
	ParentVolumeID   string `yaml:"parent_volume_id"`
	BaselineSnapshot string `yaml:"baseline_snapshot"`
	DNSName          string `yaml:"dns_name"`
	MountRoot        string `yaml:"mount_root"`
}

// ZfsStorage は zfs バックエンドの設定。
type ZfsStorage struct {
	Pool             string `yaml:"pool"`
	BaseDataset      string `yaml:"base_dataset"`
	BranchParent     string `yaml:"branch_parent"`
	BaselineSnapshot string `yaml:"baseline_snapshot"`
	Sudo             bool   `yaml:"sudo"`
}

// Engine はエンジン設定。
type Engine struct {
	Type     string         `yaml:"type"` // mysql | postgres
	Mysql    MysqlEngine    `yaml:"mysql"`
	Postgres PostgresEngine `yaml:"postgres"`
}

// PostgresEngine は postgres エンジンの設定。
// 注意: プロトコルプロキシは MySQL 専用のため、postgres ブランチへの接続は
// 直接ポート(sashiki show <name>)になる。リモート接続する場合は
// listen_addresses を広げ、base の pg_hba.conf に host 行を入れておくこと。
type PostgresEngine struct {
	PortRange       [2]int `yaml:"port_range"` // 既定 [5433, 5632]
	BinDir          string `yaml:"bin_dir"`    // 既定 /usr/lib/postgresql/16/bin
	EnvDir          string `yaml:"env_dir"`
	ListenAddresses string `yaml:"listen_addresses"` // 既定 127.0.0.1
	Sudo            bool   `yaml:"sudo"`
}

// MysqlEngine は mysql エンジンの設定。
type MysqlEngine struct {
	PortRange      [2]int `yaml:"port_range"`
	BufferPoolSize string `yaml:"buffer_pool_size"`
	ExpectedRSS    string `yaml:"expected_rss"`
	MemoryHeadroom string `yaml:"memory_headroom"`
	MaxRunning     int    `yaml:"max_running"`
	ProxyUser      string `yaml:"proxy_user"`
	ProxyPass      string `yaml:"proxy_pass"`
	EnvDir         string `yaml:"env_dir"`
	Sudo           bool   `yaml:"sudo"`
}

// Proxy はプロトコルプロキシの設定。
type Proxy struct {
	MaxConnPerBranch int `yaml:"max_conn_per_branch"`
}

// Branches はブランチのポリシー。
type Branches struct {
	NamePattern       string        `yaml:"name_pattern"`
	MaxBranches       int           `yaml:"max_branches"`
	LazyCreate        bool          `yaml:"lazy_create"`
	LazyCreateMaxWait time.Duration `yaml:"lazy_create_max_wait"`
	IdleStopAfter     time.Duration `yaml:"idle_stop_after"`
	DeleteAfterIdle   time.Duration `yaml:"delete_after_idle"`
	ReaperInterval    time.Duration `yaml:"reaper_interval"`
}

// Baseline は baseline publish のポリシー(仕様 12-3/12-4)。
type Baseline struct {
	RequireMasked    bool   `yaml:"require_masked"`
	RequireValidated bool   `yaml:"require_validated"`
	ValidatePort     int    `yaml:"validate_port"`   // validate 用の一時ポート(既定 3999)
	MaskedSentinel   string `yaml:"masked_sentinel"` // build script が touch する印(既定 /run/sashiki/baseline-masked)
}

// Hooks はフック設定。
type Hooks struct {
	Dir     string        `yaml:"dir"`
	LogDir  string        `yaml:"log_dir"`
	Timeout time.Duration `yaml:"timeout"`
}

// Auth は認証設定。
type Auth struct {
	APITokenEnv string `yaml:"api_token_env"`
}

// Default は既定値。
func Default() Config {
	return Config{
		Listen:  Listen{API: "127.0.0.1:8080", Proxy: "0.0.0.0:3306", Metrics: "127.0.0.1:9100"},
		Domain:  "sashiki.internal",
		StateDB: "/var/lib/sashiki/state.db",
		Storage: Storage{
			Backend: "ebs-zfs",
			Zfs: ZfsStorage{
				Pool:             "dbpool",
				BaseDataset:      "dbpool/base",
				BranchParent:     "dbpool/branches",
				BaselineSnapshot: "baseline",
				Sudo:             true,
			},
		},
		Engine: Engine{
			Type: "mysql",
			Mysql: MysqlEngine{
				PortRange:      [2]int{3401, 3600},
				BufferPoolSize: "256M",
				ProxyUser:      "dev",
				ProxyPass:      "dev",
				EnvDir:         "/etc/sashiki",
				Sudo:           true,
			},
			Postgres: PostgresEngine{
				PortRange:       [2]int{5433, 5632},
				EnvDir:          "/etc/sashiki",
				ListenAddresses: "127.0.0.1",
				Sudo:            true,
			},
		},
		Proxy: Proxy{MaxConnPerBranch: 50},
		Branches: Branches{
			NamePattern:       `^[a-z0-9-]{1,32}$`,
			MaxBranches:       50,
			LazyCreate:        true,
			LazyCreateMaxWait: 20 * time.Second,
			IdleStopAfter:     30 * time.Minute,
			DeleteAfterIdle:   168 * time.Hour,
			ReaperInterval:    time.Minute,
		},
		Baseline: Baseline{ValidatePort: 3999, MaskedSentinel: "/run/sashiki/baseline-masked"},
		Hooks: Hooks{
			Dir:    "/etc/sashiki/hooks",
			LogDir: "/var/log/sashiki/hooks",
		},
		Auth: Auth{APITokenEnv: "SASHIKI_API_TOKEN"},
	}
}

// Load はファイルから読み込み、既定値の上に重ねて検証する。
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// normalize は旧名(zfs / fsx)を新名(ebs-zfs / fsx-zfs)へ移す。
func (c *Config) normalize() {
	switch c.Storage.Backend {
	case "zfs":
		c.Storage.Backend = "ebs-zfs"
	case "fsx":
		c.Storage.Backend = "fsx-zfs"
	}
	if c.Storage.LegacyZfs != nil {
		c.Storage.Zfs = *c.Storage.LegacyZfs
		c.Storage.LegacyZfs = nil
	}
	if c.Storage.LegacyFsx != nil {
		c.Storage.Fsx = *c.Storage.LegacyFsx
		c.Storage.LegacyFsx = nil
	}
}

// Validate は設定の整合性チェック。
func (c Config) Validate() error {
	if c.Storage.Backend != "ebs-zfs" && c.Storage.Backend != "fsx-zfs" {
		return fmt.Errorf("storage.backend %q is not supported (ebs-zfs | fsx-zfs)", c.Storage.Backend)
	}
	if c.Storage.Backend == "fsx-zfs" {
		f := c.Storage.Fsx
		// parent_volume_id は省略可(filesystem のルートボリュームを自動発見)
		if f.Region == "" || f.FilesystemID == "" || f.BaseVolumeID == "" || f.DNSName == "" {
			return fmt.Errorf("storage.fsx-zfs requires region, filesystem_id, base_volume_id, dns_name")
		}
	}
	if c.Engine.Type != "mysql" && c.Engine.Type != "postgres" {
		return fmt.Errorf("engine.type %q is not supported (mysql | postgres)", c.Engine.Type)
	}
	if _, err := regexp.Compile(c.Branches.NamePattern); err != nil {
		return fmt.Errorf("branches.name_pattern: %w", err)
	}
	if c.Engine.Mysql.PortRange[0] <= 0 || c.Engine.Mysql.PortRange[1] < c.Engine.Mysql.PortRange[0] {
		return fmt.Errorf("engine.mysql.port_range must be [low, high]")
	}
	if c.Engine.Type == "postgres" {
		if c.Engine.Postgres.PortRange[0] <= 0 || c.Engine.Postgres.PortRange[1] < c.Engine.Postgres.PortRange[0] {
			return fmt.Errorf("engine.postgres.port_range must be [low, high]")
		}
	}
	return nil
}

// PortRange は選択中エンジンのポートレンジを返す。
func (c Config) PortRange() [2]int {
	if c.Engine.Type == "postgres" {
		return c.Engine.Postgres.PortRange
	}
	return c.Engine.Mysql.PortRange
}
