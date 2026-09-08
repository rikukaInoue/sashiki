package main

import "testing"

// majorVersion は @@version 文字列から major を取り、app_user の認証プラグイン
// 選択(major<8 は native、8.0+ は caching_sha2)の分岐に使う(#version-compat)。
func TestMajorVersion(t *testing.T) {
	cases := []struct {
		in    string
		major int
		ok    bool
	}{
		{"8.0.46", 8, true},
		{"8.4.11", 8, true},
		{"9.6.0", 9, true},
		{"5.7.44-log", 5, true},
		{"5.7.44", 5, true},
		{"10.11.5-MariaDB", 10, true}, // MariaDB でも major は取れる
		{"", 0, false},
		{"garbage", 0, false},
		{".5", 0, false},
	}
	for _, c := range cases {
		major, ok := majorVersion(c.in)
		if ok != c.ok || (ok && major != c.major) {
			t.Errorf("majorVersion(%q) = (%d, %v), want (%d, %v)", c.in, major, ok, c.major, c.ok)
		}
	}
	// プラグイン選択の意味論: 5.7 は native、8.0+ は caching_sha2 になること。
	// (5.7 は caching_sha2 プラグインが無い、8.4 は native 既定 OFF、9.x は native 廃止)
	for _, c := range []struct {
		v      string
		native bool
	}{{"5.7.44", true}, {"8.0.46", false}, {"8.4.11", false}, {"9.6.0", false}} {
		major, ok := majorVersion(c.v)
		gotNative := ok && major < 8
		if gotNative != c.native {
			t.Errorf("%s: native=%v, want %v", c.v, gotNative, c.native)
		}
	}
}
