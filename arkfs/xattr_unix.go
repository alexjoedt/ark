//go:build linux || darwin

package arkfs

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// readXattrs returns every extended attribute of path that the process can
// read. A filesystem without xattr support, or an attribute in a privileged
// namespace (security.*, trusted.*), yields nothing instead of failing the
// backup — a missing attribute is not worth losing the file over.
//
// The calls come from x/sys/unix rather than stdlib syscall: darwin has no
// xattr wrappers in stdlib.
func readXattrs(path string) (map[string][]byte, error) {
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		if isXattrSoftError(err) {
			return nil, nil //nolint:nilnil // a nil map is "no attributes"
		}
		return nil, fmt.Errorf("arkfs: list xattrs of %s: %w", path, err)
	}
	if size == 0 {
		return nil, nil //nolint:nilnil // a nil map is "no attributes"
	}

	buf := make([]byte, size)
	size, err = unix.Listxattr(path, buf)
	if err != nil {
		if isXattrSoftError(err) {
			return nil, nil //nolint:nilnil // a nil map is "no attributes"
		}
		return nil, fmt.Errorf("arkfs: list xattrs of %s: %w", path, err)
	}

	var xattrs map[string][]byte
	for name := range strings.SplitSeq(string(buf[:size]), "\x00") {
		if name == "" {
			continue
		}
		value, ok := getXattr(path, name)
		if !ok {
			continue // unreadable attribute, keep the rest
		}
		if xattrs == nil {
			xattrs = make(map[string][]byte)
		}
		xattrs[name] = value
	}
	return xattrs, nil
}

// getXattr reads one attribute. ok is false when it cannot be read, which is
// routine for security.* and trusted.* without privileges.
func getXattr(path, name string) ([]byte, bool) {
	size, err := unix.Getxattr(path, name, nil)
	if err != nil {
		return nil, false
	}
	if size == 0 {
		return []byte{}, true
	}
	buf := make([]byte, size)
	size, err = unix.Getxattr(path, name, buf)
	if err != nil {
		return nil, false
	}
	return buf[:size], true
}

// writeXattrs restores extended attributes. It is best effort: an unprivileged
// restore cannot set security.* or trusted.*, and not every target filesystem
// supports xattrs at all.
func writeXattrs(path string, xattrs map[string][]byte) {
	for name, value := range xattrs {
		_ = unix.Setxattr(path, name, value, 0)
	}
}

// isXattrSoftError reports whether err means "this filesystem or this process
// has no xattrs here", as opposed to a real I/O failure.
func isXattrSoftError(err error) bool {
	return errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.EACCES)
}
