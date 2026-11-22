package arkdocker

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arktest"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
)

// testClient connects to the local daemon or skips the test.
func testClient(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient("")
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		c.Close()
		t.Skipf("no docker daemon: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// testVolume creates an empty volume that is removed when the test ends.
func testVolume(t *testing.T, c *Client) string {
	t.Helper()
	name := fmt.Sprintf("ark-test-%d", time.Now().UnixNano())
	if _, err := c.cli.VolumeCreate(context.Background(), dockerclient.VolumeCreateOptions{Name: name}); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	t.Cleanup(func() {
		c.cli.VolumeRemove(context.Background(), name, dockerclient.VolumeRemoveOptions{Force: true})
	})
	return name
}

func TestSourceSinkConformance(t *testing.T) {
	c := testClient(t)
	arktest.RunSourceSinkTests(t, SourceTypeDockerVolume,
		func(t *testing.T, location string) ark.Source {
			return NewVolumeSource(c, location)
		},
		func(t *testing.T) (ark.Sink, string) {
			v := testVolume(t, c)
			return NewVolumeSink(c, v), v
		},
	)
}

// TestSourceYieldsWhatTheTreeCannotHoldAsUnsupported: a tar type the tree has
// no type for and a device number beyond 32 bits become unsupported entries,
// which the core counts as skipped, instead of failing the walk or passing as
// something else.
func TestSourceYieldsWhatTheTreeCannotHoldAsUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name         string
		hdr          tar.Header
		want         ark.FileType
		major, minor uint32
	}{
		{"unknown type", tar.Header{Typeflag: 'Z'}, ark.FileTypeUnsupported, 0, 0},
		{"contiguous file", tar.Header{Typeflag: tar.TypeCont}, ark.FileTypeUnsupported, 0, 0},
		{"minor beyond 32 bits", tar.Header{Typeflag: tar.TypeChar, Devminor: 1 << 32}, ark.FileTypeUnsupported, 0, 0},
		{"negative major", tar.Header{Typeflag: tar.TypeBlock, Devmajor: -1}, ark.FileTypeUnsupported, 0, 0},
		{"fifo", tar.Header{Typeflag: tar.TypeFifo}, ark.FileTypeFifo, 0, 0},
		{"char device", tar.Header{Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, ark.FileTypeCharDevice, 1, 3},
		{"block device", tar.Header{Typeflag: tar.TypeBlock, Devmajor: 7, Devminor: math.MaxUint32},
			ark.FileTypeBlockDevice, 7, math.MaxUint32},
	} {
		got := tarEntry(&tc.hdr, "entry")
		if got.Type != tc.want || got.DevMajor != tc.major || got.DevMinor != tc.minor {
			t.Errorf("%s: got %s %d,%d, want %s %d,%d", tc.name,
				got.Type, got.DevMajor, got.DevMinor, tc.want, tc.major, tc.minor)
		}
	}
}

// TestSinkSkipsSpecialFilesTheDaemonDoesNotCreate: a fifo or a device node the
// probe found the daemon does not create in the volume is reported as
// skipped, not written into a stream whose extraction would drop it silently
// or fail as a whole.
func TestSinkSkipsSpecialFilesTheDaemonDoesNotCreate(t *testing.T) {
	sink := &dockerVolumeSink{volume: "v", special: map[ark.FileType]bool{}}
	for _, typ := range []ark.FileType{ark.FileTypeFifo, ark.FileTypeCharDevice, ark.FileTypeBlockDevice} {
		err := sink.Receive(context.Background(), ark.FileInfo{Path: "node", Type: typ}, nil)
		if !errors.Is(err, ark.ErrEntrySkipped) {
			t.Errorf("Receive of a %s returned %v, want ark.ErrEntrySkipped", typ, err)
		}
	}
}

// TestSpecialFilesProbeLeavesTheVolumeEmpty: the probe reports what the
// daemon creates in the volume and removes its entries again.
func TestSpecialFilesProbeLeavesTheVolumeEmpty(t *testing.T) {
	c := testClient(t)
	v := testVolume(t, c)
	ctx := context.Background()
	if err := c.ensureHelperImage(ctx); err != nil {
		t.Fatalf("helper image: %v", err)
	}

	takes, err := c.specialFiles(ctx, v)
	if err != nil {
		t.Fatalf("specialFiles: %v", err)
	}
	t.Logf("the daemon creates in a volume: %v", takes)
	if len(takes) == 0 {
		t.Error("the probe reported on no type")
	}
	if out := runListing(t, c, v, "ls -A /data"); out != "" {
		t.Errorf("the probe left entries behind:\n%s", out)
	}
}

// runInVolume runs cmd in a helper container with the volume mounted at /data.
func runInVolume(ctx context.Context, c *Client, volume string, cmd []string) error {
	if err := c.ensureHelperImage(ctx); err != nil {
		return err
	}
	_, err := c.runContainer(ctx,
		c.helperConfig(cmd...),
		&container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: "/data"}}},
	)
	return err
}
