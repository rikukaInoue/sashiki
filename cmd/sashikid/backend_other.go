//go:build !darwin && !linux

package main

import (
	"fmt"

	"github.com/rikukaInoue/sashiki/internal/storage"
	"github.com/rikukaInoue/sashiki/internal/workspace"
)

// localBackend はローカル CoW backend(apfs / reflink)が macOS / Linux 専用の
// ため、その他の OS では未対応。
func localBackend(backend, root, baselineSnapshot string) (storage.Storage, workspace.BaselineProvider, error) {
	return nil, nil, fmt.Errorf("backend %q はこの OS では未対応です(apfs=macOS / reflink=Linux)", backend)
}
