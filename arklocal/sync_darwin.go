package arklocal

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func init() { trackWrites = true }

// syncFS asks the drive to flush its cache for the root, after Sync fsynced
// everything Put and Delete touched. macOS has no syncfs and sync(2) does not
// wait. Tests swap it to observe the calls.
var syncFS = func(root *os.File) error {
	if _, err := unix.FcntlInt(root.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		return fmt.Errorf("arklocal: F_FULLFSYNC %s: %w", root.Name(), err)
	}
	return nil
}
