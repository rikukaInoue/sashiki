//go:build !linux

package reflink

// reflink(`cp --reflink`)は Linux 前提のため、非 Linux ではスタブのみ提供する
// (macOS の開発機でクロス参照してもビルドできるようにするための最小実装)。

// Backend は非 Linux では空のスタブ。
type Backend struct{}

// New は非 Linux では機能しないスタブを返す。
func New(cfg Config) *Backend { return &Backend{} }
