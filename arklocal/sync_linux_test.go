package arklocal

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSyncFSReportsSyncfsErrors(t *testing.T) {
	origFS, origAll := syncfs, syncAll
	t.Cleanup(func() { syncfs, syncAll = origFS, origAll })
	root, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	all := 0
	syncAll = func() { all++ }

	syncfs = func(int) error { return unix.EIO }
	if err := syncFS(root); !errors.Is(err, unix.EIO) {
		t.Fatalf("syncFS with EIO = %v, want EIO", err)
	}
	if all != 0 {
		t.Fatalf("sync() ran %d times after EIO, want 0", all)
	}

	for i, errno := range []unix.Errno{unix.ENOSYS, unix.EOPNOTSUPP, unix.EINVAL} {
		syncfs = func(int) error { return errno }
		if err := syncFS(root); err != nil {
			t.Fatalf("syncFS with %v = %v, want nil", errno, err)
		}
		if all != i+1 {
			t.Fatalf("sync() ran %d times after %v, want %d", all, errno, i+1)
		}
	}
}
