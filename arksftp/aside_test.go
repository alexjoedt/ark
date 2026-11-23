package arksftp

import (
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/pkg/sftp"
)

// v3Backend serves the in-memory handlers at /repo like a server without
// posix-rename: a rename onto an existing name fails with SSH_FX_FAILURE.
// failSecond, when set, fails the rename of a temp file once an aside file
// exists; lockedAside refuses to remove an aside file.
func v3Backend(t *testing.T, failSecond error, lockedAside bool) (*Backend, *[]string) {
	t.Helper()
	hs := sftp.InMemHandler()
	cmd, list := hs.FileCmd, hs.FileList
	exists := func(p string) bool {
		_, err := list.Filelist(sftp.NewRequest("Stat", p))
		return err == nil
	}
	var calls []string
	hs.FileCmd = cmdFunc(func(r *sftp.Request) error {
		switch r.Method {
		case "Rename":
			calls = append(calls, "rename "+path.Base(r.Filepath)+" "+path.Base(r.Target))
			aside := path.Join(path.Dir(r.Target), asidePrefix+path.Base(r.Target))
			if failSecond != nil && strings.HasPrefix(path.Base(r.Filepath), tmpPrefix) && exists(aside) {
				return failSecond
			}
			if exists(r.Target) {
				return errors.New("target exists")
			}
		case "Remove":
			calls = append(calls, "remove "+path.Base(r.Filepath))
			if lockedAside && strings.HasPrefix(path.Base(r.Filepath), asidePrefix) {
				return os.ErrPermission
			}
		}
		return cmd.Filecmd(r)
	})
	return memBackend(t, hs), &calls
}

func readObject(t *testing.T, b *Backend, h ark.Handle) string {
	t.Helper()
	rc, err := b.Get(t.Context(), h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(got)
}

func asides(t *testing.T, b *Backend) []string {
	t.Helper()
	var got []string
	if err := b.ListAside(t.Context(), func(p string) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatalf("ListAside: %v", err)
	}
	return got
}

func TestPutWithoutPosixRenameRenamesTheOldObjectAside(t *testing.T) {
	h := ark.Handle{Type: ark.KeyFile, Name: "k1"}
	b, calls := v3Backend(t, nil, false)
	if err := b.Put(t.Context(), h, strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	if err := b.Put(t.Context(), h, strings.NewReader("new")); err != nil {
		t.Fatalf("Put over k1: %v", err)
	}
	if got := readObject(t, b, h); got != "new" {
		t.Errorf("k1 reads %q, want new", got)
	}
	want := "rename tmp k1, rename k1 .ark-old-k1, rename tmp k1, remove .ark-old-k1"
	if got := strings.Join(normTemp(*calls), ", "); got != want {
		t.Errorf("calls %q, want %q", got, want)
	}
	if got := asides(t, b); len(got) != 0 {
		t.Errorf("aside files %v after Put, want none", got)
	}
}

func TestPutWithoutPosixRenameRestoresTheOldObjectOnFailure(t *testing.T) {
	h := ark.Handle{Type: ark.DataPackFile, Name: "aa01"}
	b, _ := v3Backend(t, os.ErrPermission, false)
	if err := b.Put(t.Context(), h, strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(t.Context(), h, strings.NewReader("new")); err == nil {
		t.Fatal("Put with a failing rename returned nil")
	}
	if got := readObject(t, b, h); got != "old" {
		t.Errorf("aa01 reads %q after the failed Put, want old", got)
	}
	if got := asides(t, b); len(got) != 0 {
		t.Errorf("aside files %v after the failed Put, want none", got)
	}
}

// TestPutWithoutPosixRenameReplacesAStaleAsideFile pins both cases of a taken
// aside name: beside its object it is a stale copy and goes, without its
// object it is the only copy and stays.
func TestPutWithoutPosixRenameReplacesAStaleAsideFile(t *testing.T) {
	ctx := t.Context()
	b, _ := v3Backend(t, nil, false)
	stale := ark.Handle{Type: ark.KeyFile, Name: "k1"}
	lone := ark.Handle{Type: ark.KeyFile, Name: "k2"}
	if err := b.Put(ctx, stale, strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{asidePrefix + "k1", asidePrefix + "k2"} {
		f, err := b.client.Create(path.Join(b.basePath, "keys", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("older")); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	for _, h := range []ark.Handle{stale, lone} {
		if err := b.Put(ctx, h, strings.NewReader("new")); err != nil {
			t.Fatalf("Put %s: %v", h.Name, err)
		}
		if got := readObject(t, b, h); got != "new" {
			t.Errorf("%s reads %q, want new", h.Name, got)
		}
	}
	if got := strings.Join(asides(t, b), " "); got != "keys/"+asidePrefix+"k2" {
		t.Errorf("aside files %q, want only the one without its object", got)
	}
}

// TestPutWithoutPosixRenameStopsWhenTheOldObjectGoesBack pins that a rename
// failing with the generic status while the target is free, as on a full
// disk, restores the old object once and gives up instead of moving it aside
// again.
func TestPutWithoutPosixRenameStopsWhenTheOldObjectGoesBack(t *testing.T) {
	h := ark.Handle{Type: ark.KeyFile, Name: "k1"}
	b, calls := v3Backend(t, errors.New("quota exceeded"), false)
	if err := b.Put(t.Context(), h, strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	if err := b.Put(t.Context(), h, strings.NewReader("new")); err == nil {
		t.Fatal("Put with a failing rename returned nil")
	}
	if got := readObject(t, b, h); got != "old" {
		t.Errorf("k1 reads %q after the failed Put, want old", got)
	}
	want := "rename tmp k1, rename k1 .ark-old-k1, rename tmp k1, rename .ark-old-k1 k1, remove tmp"
	if got := strings.Join(normTemp(*calls), ", "); got != want {
		t.Errorf("calls %q, want %q", got, want)
	}
}

// TestPutWithoutPosixRenameReplacesBesideAnAsideFileItMayNotRemove pins that
// another user's stale aside file does not fail every Put of its object.
func TestPutWithoutPosixRenameReplacesBesideAnAsideFileItMayNotRemove(t *testing.T) {
	ctx := t.Context()
	h := ark.Handle{Type: ark.KeyFile, Name: "k1"}
	b, _ := v3Backend(t, nil, true)
	if err := b.Put(ctx, h, strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	f, err := b.client.Create(path.Join(b.basePath, "keys", asidePrefix+"k1"))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := b.Put(ctx, h, strings.NewReader("new")); err != nil {
		t.Fatalf("Put beside a locked aside file: %v", err)
	}
	if got := readObject(t, b, h); got != "new" {
		t.Errorf("k1 reads %q, want new", got)
	}
}

// normTemp replaces the random temp names in calls with "tmp".
func normTemp(calls []string) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		f := strings.Fields(c)
		for j, w := range f {
			if strings.HasPrefix(w, tmpPrefix) {
				f[j] = "tmp"
			}
		}
		out[i] = strings.Join(f, " ")
	}
	return out
}
