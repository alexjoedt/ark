package arksftp_test

import (
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexjoedt/ark"
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
	base := os.Getenv(EnvTestURI)
	if base == "" {
		t.Skipf("%s is not set, skipping the sftp conformance run", EnvTestURI)
	}

	arktest.RunBackendTests(t, func(t *testing.T) ark.Backend {
		uri := subdir(t, base)
		b, err := arkuri.Open(t.Context(), uri)
		if err != nil {
			t.Fatalf("open %s: %v", uri, err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	})
}

// subdir returns base with a fresh directory appended to its path, so each
// backend in the suite starts empty. The sftp backend creates the directory on
// the first Put.
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
