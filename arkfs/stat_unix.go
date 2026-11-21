//go:build linux || darwin

package arkfs

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/alexjoedt/ark"
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
// identity to whoever runs it. A directory keeps them: setgid there only
// passes the directory's group to new children, and nothing runs as it.
func setOwner(path string, uid, gid int, mode os.FileMode) os.FileMode {
	// Lchown works on symlinks without following them.
	if err := os.Lchown(path, uid, gid); err != nil {
		if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
			return mode
		}
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

// deviceNumbers returns the major and minor number of the device node fi.
func deviceNumbers(fi os.FileInfo) (major, minor uint32) {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	dev := uint64(stat.Rdev) //nolint:unconvert // int32 on darwin, its low bits are the number
	return unix.Major(dev), unix.Minor(dev)
}

// makeNode creates the fifo or device node of entry at path. A device without
// the privilege to make one, or a node the target filesystem cannot hold, is
// ark.ErrEntrySkipped.
func makeNode(path string, entry ark.FileInfo) error {
	perm := uint32(entry.Mode.Perm())
	var err error
	if entry.Type == ark.FileTypeFifo {
		err = unix.Mkfifo(path, perm)
	} else {
		kind := uint32(unix.S_IFBLK)
		if entry.Type == ark.FileTypeCharDevice {
			kind = unix.S_IFCHR
		}
		err = unix.Mknod(path, kind|perm, int(unix.Mkdev(entry.DevMajor, entry.DevMinor)))
	}
	switch {
	case err == nil:
		return nil
	case refusedNode(err):
		return fmt.Errorf("arkfs: create the %s %s: %w (%w)", entry.Type, path, err, ark.ErrEntrySkipped)
	default:
		return fmt.Errorf("arkfs: create the %s %s: %w", entry.Type, path, err)
	}
}

// refusedNode reports whether a mkfifo or mknod error means the node cannot
// exist here: no privilege for a device, or a filesystem such as vfat, exFAT
// or an SMB share that holds no such nodes.
func refusedNode(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP)
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
