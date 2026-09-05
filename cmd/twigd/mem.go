// メモリガード用のヘルパー。
package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// availableMem は /proc/meminfo の MemAvailable(kB)を返す。
// Linux 以外では判定不能としてエラーを返す(ガードは best-effort)。
func availableMem() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	m := regexp.MustCompile(`MemAvailable:\s+(\d+) kB`).FindSubmatch(data)
	if m == nil {
		return 0, os.ErrNotExist
	}
	kb, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		return 0, err
	}
	return kb * 1024, nil
}

// parseSize は "256M" / "1G" / "1024" をバイト数に変換する。不明なら 0。
func parseSize(s string) int64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K':
		mult, s = 1024, s[:len(s)-1]
	case 'M':
		mult, s = 1024*1024, s[:len(s)-1]
	case 'G':
		mult, s = 1024*1024*1024, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n * mult
}
