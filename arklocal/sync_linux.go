package arklocal

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// syncfs and sync are the system calls behind syncFS; tests swap them.
var (
	syncfs  = func(fd int) error { return unix.Syncfs(fd) }
	syncAll = unix.Sync
)

// syncFS flushes the filesystem that holds root. Tests swap it to observe the
// calls. Only a kernel without syncfs (ENOSYS) falls back to the global
// sync; any other error, EIO at writeback included, is returned.
var syncFS = func(root *os.File) error {
	err := syncfs(int(root.Fd()))
	if errors.Is(err, unix.ENOSYS) {
		syncAll()
		return nil
	}
	if err != nil {
		return fmt.Errorf("arklocal: syncfs %s: %w", root.Name(), err)
	}
	return nil
}
