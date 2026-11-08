package arklocal

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncFS flushes every filesystem: macOS has no syncfs, so the root is not
// consulted. Tests swap it to observe the calls.
var syncFS = func(*os.File) error { return unix.Sync() }
