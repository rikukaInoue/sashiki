package postgres

import (
	"testing"

	"github.com/rikukadev/sashiki/internal/engine"
)

// mysql 由来の "128M" 表記をそのまま渡すと postgres は
// `invalid value for parameter "shared_buffers"` で起動に失敗する(実機で発生)。
func TestPgSizeNormalizesMysqlStyleUnits(t *testing.T) {
	cases := map[string]string{
		"128M":  "128MB",
		"256m":  "256MB",
		"1G":    "1GB",
		"2g":    "2GB",
		"512kB": "512kB",
		"512k":  "512kB",
		"4T":    "4TB",
		"128MB": "128MB",
		// 単位なしは postgres だと 8kB ブロック単位になってしまうので B を付ける
		"134217728": "134217728B",
		"":          "",
	}
	for in, want := range cases {
		if got := pgSize(in); got != want {
			t.Errorf("pgSize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStartOptionsIncludesNormalizedSharedBuffers(t *testing.T) {
	e := New(Config{BinDir: "/x/bin", SharedBuffers: "128M"})
	opts := e.startOptions(engine.Instance{Branch: "pg-1", DataDir: "/d", Port: 5433})
	if !contains(opts, "shared_buffers=128MB") {
		t.Errorf("startOptions = %q, want normalized shared_buffers", opts)
	}
	// shared_buffers 未設定なら渡さない(postgres の既定に任せる)
	e2 := New(Config{BinDir: "/x/bin"})
	if contains(e2.startOptions(engine.Instance{Branch: "pg-1", DataDir: "/d", Port: 5433}), "shared_buffers") {
		t.Error("shared_buffers 未設定なら渡すべきでない")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
