package arklocal

import (
	"fmt"
	"os"
)

func init() { trackWrites = true }

// syncFS asks the drive to flush its cache for the root, after Sync fsynced
// everything Put and Delete touched. macOS has no syncfs and sync(2) does not
// wait. File.Sync is F_FULLFSYNC there and falls back to fsync on a volume
// that does not support it, such as an SMB share. Tests swap it to observe
// the calls.
var syncFS = func(root *os.File) error {
	if err := root.Sync(); err != nil {
		return fmt.Errorf("arklocal: F_FULLFSYNC %s: %w", root.Name(), err)
	}
	return nil
}
