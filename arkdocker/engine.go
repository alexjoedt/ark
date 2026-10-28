// Package arkdocker backs up Docker volumes through an ark.Repository.
//
// Engine stops the containers that use a volume, streams the volume through
// the repository and starts them again. NewVolumeSource and NewVolumeSink
// expose the same streams for callers that compose with ark.Repository
// directly. Client answers volume and container queries.
package arkdocker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	ark "github.com/alexjoedt/ark"
)

// Config holds Engine configuration.
type Config struct {
	// DockerHost overrides the Docker daemon socket address.
	// Empty string means use DOCKER_HOST env var or the default Unix socket.
	DockerHost string

	// StopTimeout is how long to wait for containers to stop gracefully,
	// and how long the restart afterwards may take. Zero means 30 seconds.
	StopTimeout time.Duration

	// SkipStop leaves the containers that use the volume running.
	SkipStop bool

	// HelperImage runs the helper containers. Empty means DefaultHelperImage.
	HelperImage string
}

// Engine orchestrates Docker container lifecycle and delegates actual
// backup/restore operations to a pkg/ark Repository.
type Engine struct {
	repo   *ark.Repository
	docker *Client
	config Config
	logger *slog.Logger

	// volumeLocks prevents concurrent backup/restore of the same volume.
	muVolumes sync.Mutex
	volumes   map[string]*sync.Mutex
}

// Option configures an Engine.
type Option func(*Engine)

// WithDockerHost sets the Docker daemon address.
func WithDockerHost(host string) Option {
	return func(e *Engine) { e.config.DockerHost = host }
}

// WithStopTimeout sets the graceful container stop timeout. Zero keeps
// the default of 30 seconds.
func WithStopTimeout(d time.Duration) Option {
	return func(e *Engine) {
		if d > 0 {
			e.config.StopTimeout = d
		}
	}
}

// WithoutStop leaves the containers that use a volume running during
// backup and restore. The caller takes the consistency risk.
func WithoutStop() Option {
	return func(e *Engine) { e.config.SkipStop = true }
}

// WithHelperImage sets the image the helper containers run, for mirrors. It
// needs a POSIX sh with rm.
func WithHelperImage(ref string) Option {
	return func(e *Engine) { e.config.HelperImage = ref }
}

// WithLogger sets the logger for the engine and its client. Default is [slog.Default]().
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) { e.logger = l }
}

// New creates a new Engine.
func New(repo *ark.Repository, opts ...Option) (*Engine, error) {
	e := &Engine{
		repo:    repo,
		config:  Config{StopTimeout: 30 * time.Second, HelperImage: DefaultHelperImage},
		logger:  slog.Default(),
		volumes: make(map[string]*sync.Mutex),
	}
	for _, o := range opts {
		o(e)
	}

	cli, err := NewClient(e.config.DockerHost)
	if err != nil {
		return nil, fmt.Errorf("arkdocker: failed to create docker client: %w", err)
	}
	cli.logger = e.logger
	if e.config.HelperImage != "" {
		cli.helperImage = e.config.HelperImage
	}
	e.docker = cli
	return e, nil
}

// BackupVolume stops the running containers that use the volume, streams
// its contents through the repository, then starts them again.
func (e *Engine) BackupVolume(ctx context.Context, volume string, opts ...ark.BackupOption) (*ark.Snapshot, error) {
	unlock := e.lockVolume(volume)
	defer unlock()

	e.logger.InfoContext(ctx, "starting volume backup", "volume", volume)

	exists, err := e.docker.VolumeExists(ctx, volume)
	if err != nil {
		return nil, fmt.Errorf("arkdocker: preflight failed for %s: %w", volume, err)
	}
	if !exists {
		return nil, fmt.Errorf("arkdocker: volume %s does not exist", volume)
	}

	var snapshot *ark.Snapshot
	err = e.withContainersStopped(ctx, volume, func(ctrs []ContainerInfo) error {
		var err error
		snapshot, err = e.repo.Backup(ctx, newVolumeSource(e.docker, volume, ctrs), opts...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("arkdocker: backup failed for volume %s: %w", volume, err)
	}

	e.logger.InfoContext(ctx, "volume backup completed",
		"volume", volume,
		"snapshot", snapshot.ID,
		"files", snapshot.Stats.FileCount,
		"size", snapshot.Stats.TotalSize,
	)
	return snapshot, nil
}

// RestoreVolume restores a snapshot into the volume, creating it when it
// does not exist and replacing its contents when it does. The running
// containers that use it are stopped first and started again after. A
// snapshot of another source type fails with ark.ErrIncompatibleSourceType.
func (e *Engine) RestoreVolume(
	ctx context.Context,
	snapshotID, volume string,
	opts ...ark.RestoreOption,
) (*ark.RestoreResult, error) {
	unlock := e.lockVolume(volume)
	defer unlock()

	e.logger.InfoContext(ctx, "starting volume restore", "volume", volume, "snapshot", snapshotID)

	if err := e.docker.ensureVolume(ctx, volume); err != nil {
		return nil, fmt.Errorf("arkdocker: preflight failed for %s: %w", volume, err)
	}

	var result *ark.RestoreResult
	err := e.withContainersStopped(ctx, volume, func([]ContainerInfo) error {
		var err error
		result, err = e.repo.Restore(ctx, snapshotID, NewVolumeSink(e.docker, volume), opts...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("arkdocker: restore failed for volume %s: %w", volume, err)
	}

	e.logger.InfoContext(ctx, "volume restore completed",
		"volume", volume,
		"snapshot", snapshotID,
		"files", result.FilesRestored,
		"bytes", result.BytesRestored,
	)
	return result, nil
}

// ListVolumes returns all Docker volumes visible to the daemon.
func (e *Engine) ListVolumes(ctx context.Context) ([]VolumeInfo, error) {
	return e.docker.ListVolumes(ctx)
}

// GetVolume returns metadata for the named Docker volume.
func (e *Engine) GetVolume(ctx context.Context, name string) (*VolumeInfo, error) {
	return e.docker.GetVolume(ctx, name)
}

// DryRunVolumeResult describes what a restore would do without executing it.
type DryRunVolumeResult struct {
	TargetVolume       string
	SnapshotID         string
	SourceName         string
	ContainersAffected []string
	WillEraseData      bool
	Warnings           []string
}

// DryRunVolume analyses a restore without making any changes.
// It checks whether the target volume exists, which containers use it, and
// whether any data would be erased — all without touching the CAS.
func (e *Engine) DryRunVolume(ctx context.Context, snapshotID, targetVolume string) (*DryRunVolumeResult, error) {
	snap, err := e.repo.Snapshot(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("arkdocker: failed to load snapshot %s: %w", snapshotID, err)
	}

	result := &DryRunVolumeResult{
		TargetVolume: targetVolume,
		SnapshotID:   snap.ID,
		SourceName:   snap.SourceName,
		Warnings:     []string{},
	}

	if _, err := e.docker.GetVolume(ctx, targetVolume); err == nil {
		result.WillEraseData = true
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("target volume %q exists — all data will be erased", targetVolume))

		containers, err := e.docker.FindContainersUsingVolume(ctx, targetVolume)
		if err == nil && len(containers) > 0 {
			for _, c := range containers {
				result.ContainersAffected = append(result.ContainersAffected, c.Name)
			}
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%d container(s) will be stopped and restarted", len(containers)))
		}
	}

	return result, nil
}

// Client returns the underlying Docker client.
func (e *Engine) Client() *Client { return e.docker }

// Close releases all resources held by the Engine.
func (e *Engine) Close() error {
	return e.docker.Close()
}

// lockVolume acquires the per-volume mutex and returns an unlock function.
// Concurrent BackupVolume/RestoreVolume calls on the same volume will block.
func (e *Engine) lockVolume(volume string) func() {
	e.muVolumes.Lock()
	mu, ok := e.volumes[volume]
	if !ok {
		mu = &sync.Mutex{}
		e.volumes[volume] = mu
	}
	e.muVolumes.Unlock()

	mu.Lock()
	return mu.Unlock
}
