package arkdocker

import (
	"context"
	"fmt"
	"time"

	dockerclient "github.com/moby/moby/client"
)

// VolumeInfo contains metadata about a Docker volume.
type VolumeInfo struct {
	Name       string
	Driver     string
	Mountpoint string
	CreatedAt  time.Time
	Labels     map[string]string
	Options    map[string]string
	Scope      string
}

// ListVolumes returns all Docker volumes.
func (c *Client) ListVolumes(ctx context.Context) ([]VolumeInfo, error) {
	resp, err := c.cli.VolumeList(ctx, dockerclient.VolumeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("arkdocker: failed to list volumes: %w", err)
	}

	result := make([]VolumeInfo, 0, len(resp.Items))
	for _, v := range resp.Items {
		result = append(
			result,
			parseVolumeInfo(v.Name, v.Driver, v.Mountpoint, v.Scope, v.CreatedAt, v.Labels, v.Options),
		)
	}
	c.logger.DebugContext(ctx, "listed volumes", "count", len(result))
	return result, nil
}

// GetVolume returns metadata for the named volume.
func (c *Client) GetVolume(ctx context.Context, name string) (*VolumeInfo, error) {
	resp, err := c.cli.VolumeInspect(ctx, name, dockerclient.VolumeInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("arkdocker: failed to inspect volume %s: %w", name, err)
	}
	v := resp.Volume
	info := parseVolumeInfo(v.Name, v.Driver, v.Mountpoint, v.Scope, v.CreatedAt, v.Labels, v.Options)
	return &info, nil
}

// VolumeExists returns true if a volume with the given name exists.
func (c *Client) VolumeExists(ctx context.Context, name string) (bool, error) {
	fa := make(dockerclient.Filters).Add("name", name)
	resp, err := c.cli.VolumeList(ctx, dockerclient.VolumeListOptions{Filters: fa})
	if err != nil {
		return false, fmt.Errorf("arkdocker: failed to check volume existence: %w", err)
	}
	for _, v := range resp.Items {
		if v.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ensureVolume creates the volume when it does not exist.
func (c *Client) ensureVolume(ctx context.Context, name string) error {
	exists, err := c.VolumeExists(ctx, name)
	if err != nil || exists {
		return err
	}
	c.logger.InfoContext(ctx, "creating volume", "volume", name)
	if _, err := c.cli.VolumeCreate(ctx, dockerclient.VolumeCreateOptions{Name: name}); err != nil {
		return fmt.Errorf("arkdocker: failed to create volume %s: %w", name, err)
	}
	return nil
}

// parseVolumeInfo converts Docker SDK volume fields to a VolumeInfo.
func parseVolumeInfo(name, driver, mountpoint, scope, createdAt string, labels, options map[string]string) VolumeInfo {
	vi := VolumeInfo{
		Name:       name,
		Driver:     driver,
		Mountpoint: mountpoint,
		Scope:      scope,
		Labels:     labels,
		Options:    options,
	}
	if createdAt != "" {
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			vi.CreatedAt = t
		}
	}
	return vi
}
