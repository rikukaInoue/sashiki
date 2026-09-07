//go:build linux

package main

import (
	"fmt"

	"github.com/rikukaInoue/sashiki/internal/storage"
	storagereflink "github.com/rikukaInoue/sashiki/internal/storage/reflink"
	"github.com/rikukaInoue/sashiki/internal/workspace"
)

// localBackend は Linux ではローカル CoW backend として reflink(cp --reflink)を返す。
func localBackend(backend, root, baselineSnapshot string) (storage.Storage, workspace.BaselineProvider, error) {
	if backend == "apfs" {
		return nil, nil, fmt.Errorf("backend %q は macOS 専用です(Linux では reflink を使ってください)", backend)
	}
	be := storagereflink.New(storagereflink.Config{Root: root, BaselineSnapshot: baselineSnapshot})
	return be, be, nil
}
