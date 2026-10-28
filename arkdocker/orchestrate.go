package arkdocker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
)

// withContainersStopped finds the containers that use the volume, stops the
// running ones, runs fn with all of them, and starts the stopped ones again.
// WithoutStop leaves them running, the lookup still happens.
func (e *Engine) withContainersStopped(ctx context.Context, volume string, fn func(ctrs []ContainerInfo) error) error {
	found, err := e.docker.FindContainersUsingVolume(ctx, volume)
	if err != nil {
		return err
	}
	ctrs, err := e.docker.inspectContainers(ctx, found)
	if err != nil {
		return err
	}
	if e.config.SkipStop {
		e.logger.InfoContext(ctx, "leaving containers running", "volume", volume)
		return fn(ctrs)
	}

	running := runningContainers(ctrs)
	if len(running) == 0 {
		return fn(ctrs)
	}

	// Deferred before stopping: when one stop fails, the ones that did stop
	// still come back. Starting a running container is a no-op.
	defer e.restartContainers(running)
	if err := e.docker.stopContainers(ctx, running, e.config.StopTimeout); err != nil {
		return err
	}
	return fn(ctrs)
}

// restartContainers starts the containers again under a context of the
// engine's own, so a cancelled caller still gets its containers back.
func (e *Engine) restartContainers(ctrs []ContainerInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), e.config.StopTimeout)
	defer cancel()
	if err := e.docker.startContainers(ctx, ctrs); err != nil {
		e.logger.Error("failed to restart containers", "error", err)
	}
	if err := e.docker.waitRunning(ctx, ctrs); err != nil {
		e.logger.Warn("containers did not come back", "error", err)
	}
}

// runningContainers keeps the containers that are running.
func runningContainers(ctrs []ContainerInfo) []ContainerInfo {
	var running []ContainerInfo
	for _, ctr := range ctrs {
		if ctr.State == string(container.StateRunning) {
			running = append(running, ctr)
		}
	}
	return running
}

// stopContainers stops every container in the list, compose managed ones
// individually like the rest. It keeps going after a failure and reports
// them all.
func (c *Client) stopContainers(ctx context.Context, ctrs []ContainerInfo, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	c.logger.InfoContext(ctx, "stopping containers", "count", len(ctrs))

	var errs []error
	for _, ctr := range ctrs {
		if _, err := c.cli.ContainerStop(ctx, ctr.ID, dockerclient.ContainerStopOptions{Timeout: &secs}); err != nil {
			errs = append(errs, fmt.Errorf("arkdocker: stop %s: %w", ctr.Name, err))
			continue
		}
		c.logger.InfoContext(ctx, "container stopped", "container", ctr.Name)
	}
	return errors.Join(errs...)
}

// startContainers starts every container in the list. It keeps going after
// a failure and reports them all.
func (c *Client) startContainers(ctx context.Context, ctrs []ContainerInfo) error {
	c.logger.InfoContext(ctx, "starting containers", "count", len(ctrs))

	var errs []error
	for _, ctr := range ctrs {
		if _, err := c.cli.ContainerStart(ctx, ctr.ID, dockerclient.ContainerStartOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("arkdocker: start %s: %w", ctr.Name, err))
			continue
		}
		c.logger.InfoContext(ctx, "container started", "container", ctr.Name)
	}
	return errors.Join(errs...)
}

// waitRunning polls until every container is running or the context ends.
func (c *Client) waitRunning(ctx context.Context, ctrs []ContainerInfo) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	pending := make(map[string]ContainerInfo, len(ctrs))
	for _, ctr := range ctrs {
		pending[ctr.ID] = ctr
	}
	for len(pending) > 0 {
		select {
		case <-ctx.Done():
			var errs []error
			for _, ctr := range pending {
				errs = append(errs, fmt.Errorf("arkdocker: %s is not running: %w", ctr.Name, ctx.Err()))
			}
			return errors.Join(errs...)
		case <-ticker.C:
			for id, ctr := range pending {
				resp, err := c.cli.ContainerInspect(ctx, id, dockerclient.ContainerInspectOptions{})
				if err != nil {
					return fmt.Errorf("arkdocker: inspect %s: %w", ctr.Name, err)
				}
				if resp.Container.State.Status == container.StateRunning {
					delete(pending, id)
				}
			}
		}
	}
	return nil
}
