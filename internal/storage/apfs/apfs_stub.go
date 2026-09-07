//go:build !darwin

package apfs

// APFS clonefile は macOS 専用のため、darwin 以外ではスタブのみ提供する
// (Linux CI が本パッケージをビルドできるようにするための最小実装)。
// 実際のバックエンドとしては使わない。

// Backend は非 darwin では空のスタブ。
type Backend struct{}

// New は非 darwin では機能しないスタブを返す。
func New(cfg Config) *Backend { return &Backend{} }
