package arksftp

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"os"
	"path"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arktest"
)

// openServerBackend connects to ARK_SFTP_TEST_URI with a fresh directory. It
// parses the URI itself because arkuri imports this package.
func openServerBackend(t *testing.T) *Backend {
	t.Helper()
	raw := os.Getenv("ARK_SFTP_TEST_URI")
	if raw == "" {
		t.Skip("ARK_SFTP_TEST_URI is not set, skipping the sftp run")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse ARK_SFTP_TEST_URI: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	q := u.Query()
	b, err := New(Config{
		Host:           u.Hostname(),
		Port:           port,
		User:           u.User.Username(),
		Path:           path.Join(u.Path, "rename-"+strconv.FormatInt(time.Now().UnixNano(), 10)),
		IdentityFile:   q.Get("identity"),
		KnownHostsFile: q.Get("known_hosts"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestConcurrentPutWithoutPosixRename(t *testing.T) {
	ctx := context.Background()
	b := openServerBackend(t)
	b.posixRename = false

	payload := bytes.Repeat([]byte("ark"), 20<<10)
	for round := range 30 {
		h := ark.Handle{Type: ark.BlobFile, Name: "abcdef01234567" + strconv.Itoa(10+round)}
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				if err := b.Put(ctx, h, bytes.NewReader(payload)); err != nil {
					errs <- err
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: Put: %v", round, err)
		}
		rc, err := b.Get(ctx, h)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("round %d: object is not one whole payload (err %v, %d bytes)", round, err, len(got))
		}
	}
}

func TestBackendConformanceWithoutPosixRename(t *testing.T) {
	openServerBackend(t) // skips without a server
	arktest.RunBackendTests(t, func(t *testing.T) ark.Backend {
		b := openServerBackend(t)
		b.posixRename = false
		return b
	})
}
