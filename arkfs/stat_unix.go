//go:build linux || darwin

package arkfs

import (
	"os"
	"syscall"
	"time"
)

func fileOwner(fi os.FileInfo) (uid, gid int) {
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid), int(stat.Gid)
	}
	return 0, 0
}

// setOwner sets the owner of path, best effort: changing it requires root.
// It returns mode as the entry may carry it afterwards. Setuid and setgid
// name the owner and the group, so they are dropped when those could not be
// set, as cp does: a restored file must not hand the restoring user's
// identity to whoever runs it.
func setOwner(path string, uid, gid int, mode os.FileMode) os.FileMode {
	// Lchown works on symlinks without following them.
	if err := os.Lchown(path, uid, gid); err != nil {
		return mode &^ (os.ModeSetuid | os.ModeSetgid)
	}
	return mode
}

// inodeKey identifies the inode behind a path. Two paths with the same key are
// names for the same file, which is what makes the second one a hardlink.
type inodeKey struct {
	dev, ino uint64
}

// fileID returns the inode key of fi, how many names that inode carries, and
// whether the platform exposes the numbers at all.
func fileID(fi os.FileInfo) (inodeKey, uint64, bool) {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return inodeKey{}, 0, false
	}
	//nolint:unconvert // field widths differ per platform
	dev, ino, nlink := uint64(stat.Dev), uint64(stat.Ino), uint64(stat.Nlink)
	return inodeKey{dev: dev, ino: ino}, nlink, true
}

// changeID returns the ctime and inode number of fi, which a backup compares
// with the parent snapshot to trust that the file is unchanged.
func changeID(fi os.FileInfo) (time.Time, uint64) {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, 0
	}
	return changeTime(stat), uint64(stat.Ino) //nolint:unconvert // field widths differ per platform
}
