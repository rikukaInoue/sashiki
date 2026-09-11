//go:build linux

// Package localstore は apfs / reflink のローカル CoW backend を config から
// 組み立てる(sashikid と baseline import CLI の両方から使う)。
package localstore

import (
	"fmt"

	"github.com/rikukadev/sashiki/internal/storage"
	storagereflink "github.com/rikukadev/sashiki/internal/storage/reflink"
)

// New は Linux ではローカル CoW backend として reflink(cp --reflink)を返す。
func New(backend, root, baselineSnapshot string) (storage.Storage, error) {
	if backend == "apfs" {
		return nil, fmt.Errorf("backend %q は macOS 専用です(Linux では reflink を使ってください)", backend)
	}
	return storagereflink.New(storagereflink.Config{Root: root, BaselineSnapshot: baselineSnapshot}), nil
}
