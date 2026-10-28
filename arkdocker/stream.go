package arkdocker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

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

// restoreVolume empties the volume, then has the daemon extract the
// uncompressed tar stream r into it through the archive endpoint of a helper
// container that never starts. Entries are relative to the volume root.
func (c *Client) restoreVolume(ctx context.Context, volume string, r io.Reader) error {
	logger := c.logger.With("volume", volume)
	logger.InfoContext(ctx, "starting streaming restore")

	ctx, cancel := context.WithTimeout(ctx, containerTimeout)
	defer cancel()

	if err := c.ensureHelperImage(ctx); err != nil {
		return err
	}

	cleanupConfig := c.helperConfig("sh", "-c", "rm -rf /data/* /data/.[!.]* /data/..?* 2>/dev/null || true")
	cleanupHostConfig := &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:   mount.TypeVolume,
			Source: volume,
			Target: "/data",
		}},
	}
	if _, err := c.runContainer(ctx, cleanupConfig, cleanupHostConfig); err != nil {
		return fmt.Errorf("arkdocker: failed to clear volume: %w", err)
	}
	logger.InfoContext(ctx, "volume cleanup completed")

	hostConfig := &container.HostConfig{
		Mounts: []mount.Mount{{
			Type:   mount.TypeVolume,
			Source: volume,
			Target: "/data",
		}},
	}
	resp, err := c.cli.ContainerCreate(
		ctx,
		dockerclient.ContainerCreateOptions{Config: c.helperConfig("true"), HostConfig: hostConfig},
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
