package storage

import (
	"errors"
	"os"
	"runtime"
	"syscall"
)

// SyncDir best-effort fsyncs a directory so that recently renamed files become durable.
// On platforms where directory fsync is unsupported, the error is ignored.
func SyncDir(dir string) error {
	if dir == "" {
		return nil
	}
	// Windows does not support directory sync; skip.
	if runtime.GOOS == "windows" {
		return nil
	}
	df, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer df.Close()
	if err := df.Sync(); err != nil {
		// Some filesystems (e.g., tmpfs) return EINVAL for directory sync; ignore in that case.
		if errors.Is(err, syscall.EINVAL) {
			return nil
		}
		return err
	}
	return nil
}
