package arksftp_test

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arksftp"
	"github.com/alexjoedt/ark/arktest"
)

// openPlainRename opens a fresh backend under root with posix-rename forced
// off.
func openPlainRename(t *testing.T, root string) *arksftp.Backend {
	t.Helper()
	b := openTestBackend(t, root).(*arksftp.Backend)
	b.DisablePosixRename()
	return b
}

func TestConcurrentPutWithoutPosixRename(t *testing.T) {
	ctx := context.Background()
	b := openPlainRename(t, testRoot(t))

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
	root := testRoot(t)
	arktest.RunBackendTests(t, func(t *testing.T) ark.Backend {
		return openPlainRename(t, root)
	})
}
