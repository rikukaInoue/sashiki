// Package reflink は Linux の reflink(CoW)バックエンド。ZFS の代わりに
// `cp --reflink=always`(XFS/Btrfs の CoW クローン)でブランチ用データを複製する。
// OrbStack のような軽量コンテナ環境では ZFS カーネルモジュールが無いため、
// カーネル組み込みの XFS reflink を CoW 基盤として使う(#113 候補B)。
// macOS の apfs バックエンド(cp -c)の Linux 版に相当する。
//
// 前提: Root が reflink 対応 FS(XFS reflink=1 / Btrfs)上にあること。
// ext4/overlay 等では `cp --reflink=always` が失敗する。
//
// レイアウトは apfs と同じ:
//
//	<Root>/base/data
//	<Root>/base/snap/<tag>
//	<Root>/branches/<name>/data
//	<Root>/branches/<name>/.init
package reflink

// Config は reflink バックエンドの設定。
type Config struct {
	Root             string // reflink 対応 FS 上のルートディレクトリ
	BaselineSnapshot string // 既定 "baseline"
	CpBin            string // 既定 "cp"
}
