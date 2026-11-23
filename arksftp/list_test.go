package arksftp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
)

// TestListSkipsNamesCheckNameRefuses plants files another tool leaves in a
// namespace and in a pack shard. List must not yield them: Get and Delete
// refuse their names, so a caller could neither read nor remove them.
func TestListSkipsNamesCheckNameRefuses(t *testing.T) {
	ctx := context.Background()
	b := pipeBackend(t)
	lock := ark.Handle{Type: ark.LockFile, Name: "l1"}
	pack := ark.Handle{Type: ark.DataPackFile, Name: "aa01"}
	for _, h := range []ark.Handle{lock, pack} {
		if err := b.Put(ctx, h, strings.NewReader("x")); err != nil {
			t.Fatalf("Put %s: %v", h.Name, err)
		}
	}
	for _, p := range []string{
		filepath.Join(b.basePath, "locks", ".DS_Store"),
		filepath.Join(b.basePath, "datapacks", ".DS_Store"),
		filepath.Join(b.basePath, "datapacks", "aa", "._aa01"),
	} {
		if err := os.WriteFile(p, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []ark.Handle{lock, pack} {
		var names []string
		if err := b.List(ctx, h.Type, func(i ark.BlobInfo) error {
			names = append(names, i.Name)
			return nil
		}); err != nil {
			t.Fatalf("List %v: %v", h.Type, err)
		}
		if len(names) != 1 || names[0] != h.Name {
			t.Errorf("List %v = %v, want [%s]", h.Type, names, h.Name)
		}
	}
}
