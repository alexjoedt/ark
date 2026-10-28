package arkdocker

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
)

// helperLabel marks the containers arkdocker starts itself. Queries skip
// them, so a backup never counts its own tar container as one to stop.
const helperLabel = "com.alexjoedt.ark.helper"

// ContainerInfo describes a container.
type ContainerInfo struct {
	ID             string            // Container ID
	Name           string            // Container name without the leading slash
	State          string            // running, exited, ...
	Status         string            // Human readable status
	Image          string            // Image name
	ImageID        string            // Image ID
	Labels         map[string]string // Container labels
	ComposeProject string            // Compose project name, from the labels
	ComposeService string            // Compose service name, from the labels
	Mounts         []string          // Names of the mounted volumes
}

// FindContainersUsingVolume lists the containers, running or not, that
// mount the volume. arkdocker's own helper containers are left out.
func (c *Client) FindContainersUsingVolume(ctx context.Context, volume string) ([]ContainerInfo, error) {
	ctrs, err := c.ListContainers(ctx, true)
	if err != nil {
		return nil, err
	}
	var result []ContainerInfo
	for _, ctr := range ctrs {
		if slices.Contains(ctr.Mounts, volume) {
			result = append(result, ctr)
		}
	}
	c.logger.DebugContext(ctx, "found containers using volume", "volume", volume, "count", len(result))
	return result, nil
}

// inspectContainers refreshes the containers whose listed image degraded to
// an image ID, since inspect keeps the reference the container was created
// with. The others are returned as listed. A container removed in the meantime is dropped.
func (c *Client) inspectContainers(ctx context.Context, ctrs []ContainerInfo) ([]ContainerInfo, error) {
	result := make([]ContainerInfo, 0, len(ctrs))
	for _, ctr := range ctrs {
		if !looksLikeImageID(ctr.Image) {
			result = append(result, ctr)
			continue
		}
		info, err := c.GetContainer(ctx, ctr.ID)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				c.logger.DebugContext(ctx, "container vanished before inspect", "container", ctr.Name)
				continue
			}
			return nil, err
		}
		result = append(result, *info)
	}
	return result, nil
}

// ListContainers lists the containers, only the running ones unless all is
// set. arkdocker's own helper containers are left out.
func (c *Client) ListContainers(ctx context.Context, all bool) ([]ContainerInfo, error) {
	resp, err := c.cli.ContainerList(ctx, dockerclient.ContainerListOptions{All: all})
	if err != nil {
		return nil, fmt.Errorf("arkdocker: failed to list containers: %w", err)
	}

	result := make([]ContainerInfo, 0, len(resp.Items))
	for _, ctr := range resp.Items {
		if _, helper := ctr.Labels[helperLabel]; helper {
			continue
		}
		var mounts []string
		for _, m := range ctr.Mounts {
			mounts = append(mounts, m.Name)
		}
		result = append(
			result,
			containerInfo(
				ctr.ID,
				ctr.Names[0],
				string(ctr.State),
				ctr.Status,
				ctr.Image,
				ctr.ImageID,
				ctr.Labels,
				mounts,
			),
		)
	}
	return result, nil
}

// GetContainer describes one container by name or ID.
func (c *Client) GetContainer(ctx context.Context, id string) (*ContainerInfo, error) {
	resp, err := c.cli.ContainerInspect(ctx, id, dockerclient.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("arkdocker: failed to inspect container %s: %w", id, err)
	}
	inspect := resp.Container

	var mounts []string
	for _, m := range inspect.Mounts {
		if m.Name != "" {
			mounts = append(mounts, m.Name)
		}
	}
	state := string(inspect.State.Status)
	info := containerInfo(
		inspect.ID,
		inspect.Name,
		state,
		state,
		inspect.Config.Image,
		inspect.Image,
		inspect.Config.Labels,
		mounts,
	)
	return &info, nil
}

// ContainerExists reports whether a container with the name or ID exists.
func (c *Client) ContainerExists(ctx context.Context, nameOrID string) (bool, error) {
	fa := make(dockerclient.Filters).Add("name", nameOrID).Add("id", nameOrID)
	resp, err := c.cli.ContainerList(ctx, dockerclient.ContainerListOptions{All: true, Filters: fa})
	if err != nil {
		return false, fmt.Errorf("arkdocker: failed to check container existence: %w", err)
	}
	return len(resp.Items) > 0, nil
}

func containerInfo(
	id, name, state, status, image, imageID string,
	labels map[string]string,
	mounts []string,
) ContainerInfo {
	return ContainerInfo{
		ID:             id,
		Name:           strings.TrimPrefix(name, "/"),
		State:          state,
		Status:         status,
		Image:          image,
		ImageID:        imageID,
		Labels:         labels,
		ComposeProject: labels["com.docker.compose.project"],
		ComposeService: labels["com.docker.compose.service"],
		Mounts:         mounts,
	}
}

// helperConfig is the container config every helper container starts from.
func (c *Client) helperConfig(cmd ...string) *container.Config {
	return &container.Config{
		Image:      c.helperImage,
		Cmd:        cmd,
		WorkingDir: "/",
		Labels:     map[string]string{helperLabel: "1"},
	}
}

var imageIDPattern = regexp.MustCompile(`^(sha256:)?[a-f0-9]{12,64}$`)

// looksLikeImageID reports whether s is an image ID, full or short, rather
// than a name.
func looksLikeImageID(s string) bool { return imageIDPattern.MatchString(s) }
