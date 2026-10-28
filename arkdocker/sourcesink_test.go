package arkdocker

import (
	"context"
	"fmt"
	"io"
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

// A fifo in the volume comes back as unsupported, so the core counts it
// instead of the source dropping it silently.
func TestSourceYieldsSpecialFilesAsUnsupported(t *testing.T) {
	c := testClient(t)
	v := testVolume(t, c)
	ctx := context.Background()

	fixture := []string{"sh", "-c", "mkfifo /data/pipe && echo x > /data/file"}
	if err := runInVolume(ctx, c, v, fixture); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	got := map[string]ark.FileType{}
	err := NewVolumeSource(c, v).Walk(ctx, func(info ark.FileInfo, r io.Reader) error {
		got[info.Path] = info.Type
		if r != nil {
			io.Copy(io.Discard, r)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got["pipe"] != ark.FileTypeUnsupported {
		t.Errorf("pipe came back as %q, want %q", got["pipe"], ark.FileTypeUnsupported)
	}
	if got["file"] != ark.FileTypeRegular {
		t.Errorf("file came back as %q, want %q", got["file"], ark.FileTypeRegular)
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
