package arkfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexjoedt/ark"
)

// TestCopyFallbackSetsTheOwnerBeforeTheMode: chown clears setuid and setgid,
// so the copy that stands in for a hardlink sets the mode last, as Receive
// does for a regular file.
func TestCopyFallbackSetsTheOwnerBeforeTheMode(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mode := os.ModeSetuid | 0o755
	if err := copyFile(src, dst, ark.FileInfo{Mode: mode, UID: os.Getuid(), GID: os.Getgid()}); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if got := fi.Mode() &^ os.ModeType; got != mode {
		t.Errorf("the copy has mode %v, want %v", got, mode)
	}
}

// TestSinkDropsSetuidAndSetgidWithoutTheOwner: an unprivileged restore cannot
// give an entry to another user, and a setuid file left behind would run as
// whoever restored it. The sticky bit names no owner and stays, and a
// directory keeps all three: nothing runs as it.
func TestSinkDropsSetuidAndSetgidWithoutTheOwner(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can set any owner")
	}
	ctx := context.Background()
	root := t.TempDir()
	sink, err := NewSink(root)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if err := sink.Open(ctx, ark.SourceMeta{Type: ark.SourceTypeFile}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	special := os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	for _, e := range []ark.FileInfo{
		{Path: "dir", Type: ark.FileTypeDirectory, Mode: special | 0o775},
		{Path: "file", Type: ark.FileTypeRegular, Mode: os.ModeSetuid | os.ModeSetgid | 0o755},
	} {
		if err := sink.Receive(ctx, e, nil); err != nil {
			t.Fatalf("Receive(%s): %v", e.Path, err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for name, want := range map[string]os.FileMode{"dir": special | 0o775, "file": 0o755} {
		fi, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		if got := fi.Mode() &^ os.ModeType; got != want {
			t.Errorf("%s of root restored with mode %v, want %v", name, got, want)
		}
	}
}
