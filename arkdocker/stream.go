package arkdocker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
)

// DefaultHelperImage is the image the helper containers run. Backup and
// restore move data through the daemon's archive endpoint, so the image only
// runs the shell that empties a volume before a restore.
const DefaultHelperImage = "busybox:stable"

const containerTimeout = 30 * time.Minute

// volumeStream is the tar stream of a volume. Close removes the helper
// container behind it, so a caller that returns after Close leaves nothing
// behind.
type volumeStream struct {
	io.ReadCloser
	cleanup func()
}

func (s *volumeStream) Close() error {
	err := s.ReadCloser.Close()
	s.cleanup()
	return err
}

// streamVolume creates a helper container with the volume mounted read only
// and returns the daemon's tar archive of the mount, entries rooted at
// "data/". The container never starts: the archive endpoint streams far
// faster than the stdout of a container running tar, which goes through the
// attach stream. The caller reads and closes the stream; the container is
// removed on Close.
func (c *Client) streamVolume(ctx context.Context, volume string) (io.ReadCloser, error) {
	logger := c.logger.With("volume", volume)
	logger.InfoContext(ctx, "starting streaming backup")

	ctx, cancel := context.WithTimeout(ctx, containerTimeout)

	if err := c.ensureHelperImage(ctx); err != nil {
		cancel()
		return nil, err
	}

	hostConfig := &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:     mount.TypeVolume,
			Source:   volume,
			Target:   "/data",
			ReadOnly: true,
		}},
	}
	resp, err := c.cli.ContainerCreate(
		ctx,
		dockerclient.ContainerCreateOptions{Config: c.helperConfig("true"), HostConfig: hostConfig},
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("arkdocker: failed to create stream container: %w", err)
	}
	containerID := resp.ID
	cleanup := func() {
		c.removeContainer(containerID)
		cancel()
	}

	res, err := c.cli.CopyFromContainer(ctx, containerID, dockerclient.CopyFromContainerOptions{SourcePath: "/data"})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("arkdocker: failed to read volume archive: %w", err)
	}
	logger.InfoContext(ctx, "volume archive stream opened", "container_id", containerID[:12])

	return &volumeStream{ReadCloser: res.Content, cleanup: cleanup}, nil
}

// volumeMount mounts the volume read-write at /data.
func volumeMount(volume string) *container.HostConfig {
	return &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:   mount.TypeVolume,
			Source: volume,
			Target: "/data",
		}},
	}
}

// clearVolume removes everything the volume holds.
func (c *Client) clearVolume(ctx context.Context, volume string) error {
	logger := c.logger.With("volume", volume)
	logger.InfoContext(ctx, "starting streaming restore")

	ctx, cancel := context.WithTimeout(ctx, containerTimeout)
	defer cancel()

	if err := c.ensureHelperImage(ctx); err != nil {
		return err
	}

	cleanupConfig := c.helperConfig("sh", "-c", "rm -rf /data/* /data/.[!.]* /data/..?* 2>/dev/null || true")
	if _, err := c.runContainer(ctx, cleanupConfig, volumeMount(volume)); err != nil {
		return fmt.Errorf("arkdocker: failed to clear volume: %w", err)
	}
	logger.InfoContext(ctx, "volume cleanup completed")
	return nil
}

// extractVolume has the daemon extract the uncompressed tar stream r into
// the volume through the archive endpoint of a helper container that never
// starts. Entries are relative to the volume root.
func (c *Client) extractVolume(ctx context.Context, volume string, r io.Reader) error {
	logger := c.logger.With("volume", volume)

	ctx, cancel := context.WithTimeout(ctx, containerTimeout)
	defer cancel()

	resp, err := c.cli.ContainerCreate(
		ctx,
		dockerclient.ContainerCreateOptions{Config: c.helperConfig("true"), HostConfig: volumeMount(volume)},
	)
	if err != nil {
		return fmt.Errorf("arkdocker: failed to create restore container: %w", err)
	}
	defer c.removeContainer(resp.ID)

	// CopyUIDGID stays false: it would chown every entry to the container
	// user instead of keeping the owners the tar headers carry.
	if _, err := c.cli.CopyToContainer(ctx, resp.ID, dockerclient.CopyToContainerOptions{
		DestinationPath: "/data",
		Content:         r,
	}); err != nil {
		return fmt.Errorf("arkdocker: failed to write tar stream to volume: %w", err)
	}

	logger.InfoContext(ctx, "streaming restore completed")
	return nil
}

// specialFiles reports which of fifo, character device and block device the
// daemon creates in the volume. It extracts one of each through the archive
// endpoint, the way a restore does, and looks for it afterwards: a daemon in
// a user namespace leaves out a node it may not create without an error, and
// a filesystem that cannot hold one fails the extraction. The probe entries
// are removed again, so the volume must not be cleared concurrently.
func (c *Client) specialFiles(ctx context.Context, volume string) (map[ark.FileType]bool, error) {
	logger := c.logger.With("volume", volume)

	ctx, cancel := context.WithTimeout(ctx, containerTimeout)
	defer cancel()

	resp, err := c.cli.ContainerCreate(
		ctx,
		dockerclient.ContainerCreateOptions{Config: c.helperConfig("true"), HostConfig: volumeMount(volume)},
	)
	if err != nil {
		return nil, fmt.Errorf("arkdocker: failed to create probe container: %w", err)
	}
	defer c.removeContainer(resp.ID)

	// A random name keeps the probe off the entries the restore writes.
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	prefix := ".ark-probe-" + hex.EncodeToString(suffix[:]) + "-"

	// Mode 0 on each; the block device 0,0 names no driver.
	probes := []ark.FileInfo{
		{Type: ark.FileTypeFifo},
		{Type: ark.FileTypeCharDevice, DevMajor: 1, DevMinor: 3},
		{Type: ark.FileTypeBlockDevice},
	}
	takes := make(map[ark.FileType]bool, len(probes))
	remove := []string{"rm", "-f"}
	for _, probe := range probes {
		probe.Path = prefix + string(probe.Type)
		remove = append(remove, "/data/"+probe.Path)

		var archive bytes.Buffer
		tw := tar.NewWriter(&archive)
		if err := tw.WriteHeader(fileInfoToTarHeader(probe)); err != nil {
			return nil, fmt.Errorf("arkdocker: probe header: %w", err)
		}
		if err := tw.Close(); err != nil {
			return nil, fmt.Errorf("arkdocker: probe archive: %w", err)
		}
		if _, err := c.cli.CopyToContainer(ctx, resp.ID, dockerclient.CopyToContainerOptions{
			DestinationPath: "/data",
			Content:         &archive,
		}); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			logger.InfoContext(ctx, "volume refuses a special file", "type", probe.Type, "error", err)
			continue
		}
		stat := dockerclient.ContainerStatPathOptions{Path: "/data/" + probe.Path}
		_, err := c.cli.ContainerStatPath(ctx, resp.ID, stat)
		takes[probe.Type] = err == nil
	}
	if _, err := c.runContainer(ctx, c.helperConfig(remove...), volumeMount(volume)); err != nil {
		return nil, fmt.Errorf("arkdocker: failed to remove the probe entries: %w", err)
	}
	return takes, nil
}

// ensureHelperImage pulls the helper image when it is not present locally.
func (c *Client) ensureHelperImage(ctx context.Context) error {
	if _, err := c.cli.ImageInspect(ctx, c.helperImage); err == nil {
		return nil
	}
	c.logger.InfoContext(ctx, "pulling helper image", "image", c.helperImage)
	reader, err := c.cli.ImagePull(ctx, c.helperImage, dockerclient.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("arkdocker: failed to pull image %s: %w", c.helperImage, err)
	}
	defer reader.Close()
	if err := reader.Wait(ctx); err != nil {
		return fmt.Errorf("arkdocker: failed to pull image %s: %w", c.helperImage, err)
	}
	return nil
}

// runContainer creates and starts a container, waits for it to exit and
// removes it. It returns the container's combined output. The removal is
// explicit rather than AutoRemove, which lets go of the volume only after
// the daemon gets around to it.
func (c *Client) runContainer(
	ctx context.Context,
	cfg *container.Config,
	hostCfg *container.HostConfig,
) (string, error) {
	resp, err := c.cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg})
	if err != nil {
		return "", fmt.Errorf("arkdocker: failed to create container: %w", err)
	}
	containerID := resp.ID
	defer c.removeContainer(containerID)

	if _, err := c.cli.ContainerStart(ctx, containerID, dockerclient.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("arkdocker: failed to start container: %w", err)
	}
	c.logger.InfoContext(ctx, "container started", "id", containerID[:12])

	if err := c.waitContainer(ctx, containerID, c.logger); err != nil {
		return "", fmt.Errorf("arkdocker: container: %w", err)
	}
	out, err := c.containerLogs(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("arkdocker: container: %w", err)
	}
	return out, nil
}

// waitContainer blocks until the container stops. A non-zero exit is an
// error carrying the container's output in the log.
func (c *Client) waitContainer(ctx context.Context, containerID string, logger *slog.Logger) error {
	wait := c.cli.ContainerWait(
		ctx,
		containerID,
		dockerclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning},
	)
	select {
	case err := <-wait.Error:
		if err != nil {
			return fmt.Errorf("wait: %w", err)
		}
		return nil
	case status := <-wait.Result:
		if status.StatusCode != 0 {
			if logs, err := c.containerLogs(ctx, containerID); err == nil && logs != "" {
				logger.ErrorContext(ctx, "container failed", "logs", logs)
			}
			return fmt.Errorf("exited with status %d", status.StatusCode)
		}
		return nil
	case <-ctx.Done():
		_, _ = c.cli.ContainerStop(
			context.Background(),
			containerID,
			dockerclient.ContainerStopOptions{},
		)
		return fmt.Errorf("timed out: %w", ctx.Err())
	}
}

// removeContainer force removes a container, best effort, off the caller's context.
func (c *Client) removeContainer(containerID string) {
	_, _ = c.cli.ContainerRemove(
		context.Background(),
		containerID,
		dockerclient.ContainerRemoveOptions{Force: true},
	)
}

// containerLogs returns the combined stdout and stderr of a container.
func (c *Client) containerLogs(ctx context.Context, containerID string) (string, error) {
	reader, err := c.cli.ContainerLogs(
		ctx,
		containerID,
		dockerclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true},
	)
	if err != nil {
		return "", fmt.Errorf("logs: %w", err)
	}
	defer reader.Close()

	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, reader); err != nil {
		return "", fmt.Errorf("logs: %w", err)
	}
	return out.String(), nil
}
