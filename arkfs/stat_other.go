//go:build !unix

package arkfs

import "os"

func fileOwner(fi os.FileInfo) (uid, gid int) { return 0, 0 }

func setOwner(path string, uid, gid int) {} // not supported

type inodeKey struct{ dev, ino uint64 }

func fileID(os.FileInfo) (inodeKey, uint64, bool) { return inodeKey{}, 0, false }
