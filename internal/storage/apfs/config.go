// Package apfs は macOS ネイティブの CoW バックエンド。ZFS の代わりに APFS の
// clonefile(cp -c)でブランチ用データディレクトリを一瞬・省容量で複製する。
// フル VM も ZFS カーネル拡張も不要で Mac 上で直接動かすためのもの(#113 候補A)。
//
// レイアウト:
//
//	<Root>/base/data          ベースの datadir
//	<Root>/base/snap/<tag>    ベースライン snapshot(data の CoW クローン、immutable 扱い)
//	<Root>/branches/<name>/data   ブランチの datadir(baseline snapshot のクローン)
//	<Root>/branches/<name>/.init  作成直後の CoW クローン(reset 用)
package apfs

// Config は apfs バックエンドの設定。
type Config struct {
	Root             string // すべての実体を置くルートディレクトリ(APFS 上)
	BaselineSnapshot string // 既定 "baseline"(<Root>/base/snap/<name>)
	CpBin            string // 既定 "cp"。テストで差し替える
}
