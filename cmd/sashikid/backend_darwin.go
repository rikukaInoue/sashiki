//go:build darwin

package main

import (
	"fmt"

	"github.com/rikukaInoue/sashiki/internal/storage"
	storageapfs "github.com/rikukaInoue/sashiki/internal/storage/apfs"
	"github.com/rikukaInoue/sashiki/internal/workspace"
)

// localBackend は macOS ではローカル CoW backend として apfs(clonefile)を返す。
func localBackend(backend, root, baselineSnapshot string) (storage.Storage, workspace.BaselineProvider, error) {
	if backend == "reflink" {
		return nil, nil, fmt.Errorf("backend %q は Linux 専用です(macOS では apfs を使ってください)", backend)
	}
	be := storageapfs.New(storageapfs.Config{Root: root, BaselineSnapshot: baselineSnapshot})
	return be, be, nil
}
