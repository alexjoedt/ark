package arkdocker

import (
	"context"
	"fmt"
	"log/slog"

	dockerclient "github.com/moby/moby/client"
)

// Client answers volume and container queries and runs the helper containers
// behind the volume source and sink. Engine.Client returns the engine's.
type Client struct {
	cli         *dockerclient.Client
	logger      *slog.Logger
	host        string
	helperImage string
}

// NewClient connects to the daemon at dockerHost. Empty means DOCKER_HOST
// and friends, then the default socket.
func NewClient(dockerHost string) (*Client, error) {
	// API version negotiation is on by default, so Docker 20.10 and newer work.
	opts := []dockerclient.Opt{dockerclient.FromEnv}
	if dockerHost != "" {
		opts = append(opts, dockerclient.WithHost(dockerHost))
	}

	cli, err := dockerclient.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("arkdocker: failed to create docker client: %w", err)
	}
	return &Client{
		cli:         cli,
		logger:      slog.Default(),
		host:        dockerHost,
		helperImage: DefaultHelperImage,
	}, nil
}

// Ping verifies connectivity to the Docker daemon.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.cli.Ping(ctx, dockerclient.PingOptions{}); err != nil {
		return fmt.Errorf("arkdocker: docker daemon not reachable: %w", err)
	}
	return nil
}

// ServerVersion returns the Docker daemon server version string.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	info, err := c.cli.Info(ctx, dockerclient.InfoOptions{})
	if err != nil {
		return "", fmt.Errorf("arkdocker: failed to query docker info: %w", err)
	}
	return info.Info.ServerVersion, nil
}

// Close closes the Docker client connection.
func (c *Client) Close() error {
	if c.cli != nil {
		return c.cli.Close()
	}
	return nil
}
