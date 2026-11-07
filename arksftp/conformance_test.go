package arksftp_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arksftp"
	"github.com/alexjoedt/ark/arktest"
	"github.com/alexjoedt/ark/arkuri"
)

// EnvTestURI names the server the conformance run uses, for example
// sftp://ark@127.0.0.1:2222/home/ark/repo?identity=/tmp/id&known_hosts=/tmp/kh.
// Without it the run is skipped, so the suite stays green off CI.
const EnvTestURI = "ARK_SFTP_TEST_URI"

// seq numbers the repository directories so no two backends share one.
var seq atomic.Uint64

// TestBackendConformance runs the shared backend suite against a real SFTP
// server. Unit tests cover host keys, auth and ssh_config; this is the only
// place where atomic rename and concurrency meet an actual server.
func TestBackendConformance(t *testing.T) {
	root := testRoot(t)
	arktest.RunBackendTests(t, func(t *testing.T) ark.Backend {
		return openTestBackend(t, root)
	})
}

// testRoot returns EnvTestURI with a fresh directory appended and removes
// that directory, with every backend opened under it, when t ends. It skips t
// when EnvTestURI is unset.
func testRoot(t *testing.T) string {
	t.Helper()
	base := os.Getenv(EnvTestURI)
	if base == "" {
		t.Skipf("%s is not set, skipping the sftp run", EnvTestURI)
	}
	root := subdir(t, base)
	t.Cleanup(func() {
		b, err := arkuri.Open(context.Background(), root)
		if err != nil {
			t.Errorf("open %s for cleanup: %v", root, err)
			return
		}
		defer func() { _ = b.Close() }()
		if err := b.(*arksftp.Backend).RemoveRoot(); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove %s: %v", root, err)
		}
	})
	return root
}

// openTestBackend opens a backend in a fresh directory under root and closes
// it when t ends. The directory is created on the first Put.
func openTestBackend(t *testing.T, root string) ark.Backend {
	t.Helper()
	uri := subdir(t, root)
	b, err := arkuri.Open(t.Context(), uri)
	if err != nil {
		t.Fatalf("open %s: %v", uri, err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// subdir returns base with a fresh directory appended to its path.
func subdir(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s=%q: %v", EnvTestURI, base, err)
	}
	name := strings.NewReplacer("/", "-", " ", "_").Replace(t.Name())
	u.Path = path.Join(u.Path, name+"-"+strconv.FormatUint(seq.Add(1), 10))
	return u.String()
}
