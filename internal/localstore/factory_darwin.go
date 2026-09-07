//go:build darwin

package localstore

import (
	"fmt"

	"github.com/rikukaInoue/sashiki/internal/storage"
	storageapfs "github.com/rikukaInoue/sashiki/internal/storage/apfs"
)

// New は macOS ではローカル CoW backend として apfs(clonefile)を返す。
func New(backend, root, baselineSnapshot string) (storage.Storage, error) {
	if backend == "reflink" {
		return nil, fmt.Errorf("backend %q は Linux 専用です(macOS では apfs を使ってください)", backend)
	}
	return storageapfs.New(storageapfs.Config{Root: root, BaselineSnapshot: baselineSnapshot}), nil
}
