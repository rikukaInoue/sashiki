// Package config は /etc/twig/config.yaml の読み込み(仕様 12-1 の v0.1 サブセット)。
// シークレットは設定ファイルに書かない。API トークンは環境変数から読む。
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config は twigd 全体の設定。
type Config struct {
	Listen   Listen   `yaml:"listen"`
	Domain   string   `yaml:"domain"`
	StateDB  string   `yaml:"state_db"`
	Storage  Storage  `yaml:"storage"`
	Engine   Engine   `yaml:"engine"`
	Proxy    Proxy    `yaml:"proxy"`
	Branches Branches `yaml:"branches"`
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
	Backend string     `yaml:"backend"` // zfs | fsx
	Zfs     ZfsStorage `yaml:"zfs"`
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
	Type  string      `yaml:"type"` // mysql
	Mysql MysqlEngine `yaml:"mysql"`
}

// MysqlEngine は mysql エンジンの設定。
type MysqlEngine struct {
	PortRange      [2]int `yaml:"port_range"`
	BufferPoolSize string `yaml:"buffer_pool_size"`
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
	NamePattern string `yaml:"name_pattern"`
	MaxBranches int    `yaml:"max_branches"`
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
		Domain:  "twig.internal",
		StateDB: "/var/lib/twig/state.db",
		Storage: Storage{
			Backend: "zfs",
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
				EnvDir:         "/etc/twig",
				Sudo:           true,
			},
		},
		Proxy: Proxy{MaxConnPerBranch: 50},
		Branches: Branches{
			NamePattern: `^[a-z0-9-]{1,32}$`,
			MaxBranches: 50,
		},
		Hooks: Hooks{
			Dir:    "/etc/twig/hooks",
			LogDir: "/var/log/twig/hooks",
		},
		Auth: Auth{APITokenEnv: "TWIG_API_TOKEN"},
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
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate は設定の整合性チェック。
func (c Config) Validate() error {
	if c.Storage.Backend != "zfs" {
		return fmt.Errorf("storage.backend %q is not supported in v0.1 (zfs only)", c.Storage.Backend)
	}
	if c.Engine.Type != "mysql" {
		return fmt.Errorf("engine.type %q is not supported in v0.1 (mysql only)", c.Engine.Type)
	}
	if _, err := regexp.Compile(c.Branches.NamePattern); err != nil {
		return fmt.Errorf("branches.name_pattern: %w", err)
	}
	if c.Engine.Mysql.PortRange[0] <= 0 || c.Engine.Mysql.PortRange[1] < c.Engine.Mysql.PortRange[0] {
		return fmt.Errorf("engine.mysql.port_range must be [low, high]")
	}
	return nil
}
