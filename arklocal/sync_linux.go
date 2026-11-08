package arklocal

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncFS flushes the filesystem that holds root. Tests swap it to observe the
// calls. syncfs can fail on a filesystem that does not implement it; the
// global sync then flushes every filesystem, which is slower but never
// silently skipped.
var syncFS = func(root *os.File) error {
	if err := unix.Syncfs(int(root.Fd())); err != nil {
		unix.Sync()
	}
	return nil
}
