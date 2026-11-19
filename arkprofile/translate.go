package arkprofile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkuri"
)

// sourceTypeDockerVolume is arkdocker.SourceTypeDockerVolume, which
// arkprofile cannot import without the Docker SDK; a test in cmd/ark pins the
// two together.
const sourceTypeDockerVolume ark.SourceType = "docker-volume"

// sourceName is the source name of the profile's snapshots: backup.volume,
// else backup.source, else the absolute backup.path as [arkfs.NewSource]
// names it, the name a plain backup of that path stores. None set gives "".
// It cannot ask arkfs, which needs the directory to exist, and a forget must
// work without it; TestSourceNames pins the two names together.
func (p *Profile) sourceName() (string, error) {
	switch {
	case p.Backup.Volume != "":
		return p.Backup.Volume, nil
	case p.Backup.Source != "":
		return p.Backup.Source, nil
	case p.Backup.Path == "":
		return "", nil
	}
	abs, err := filepath.Abs(p.Backup.Path)
	if err != nil {
		return "", fmt.Errorf("resolve backup.path %q: %w", p.Backup.Path, err)
	}
	return abs, nil
}

// host is the host the profile's snapshots record: backup.host, else the
// machine's hostname as the CLI's --host defaults to it.
func (p *Profile) host() string {
	if p.Backup.Host != "" {
		return p.Backup.Host
	}
	h, _ := os.Hostname()
	return h
}

// Source returns the arkfs source of backup.path with the profile's excludes,
// exclude files and cloud-file option, named backup.source, else the absolute
// backup.path. A volume profile has no arkfs source and fails; its caller
// builds the source with arkdocker.
func (p *Profile) Source() (ark.Source, error) {
	b := p.Backup
	if b.Volume != "" {
		return nil, invalidf("backup.volume %s: a docker volume source comes from arkdocker, not from Source", b.Volume)
	}
	if b.Path == "" {
		return nil, invalidf("backup.path is empty")
	}
	var opts []arkfs.SourceOption
	if b.Source != "" {
		opts = append(opts, arkfs.WithName(b.Source))
	}
	if len(b.Excludes) > 0 {
		opts = append(opts, arkfs.WithExclude(b.Excludes...))
	}
	for _, f := range b.ExcludeFiles {
		opts = append(opts, arkfs.WithExcludeFile(f))
	}
	if b.ExcludeCloudFiles {
		opts = append(opts, arkfs.WithExcludeCloudFiles())
	}
	return arkfs.NewSource(b.Path, opts...)
}

// BackupOptions returns the tags, labels, host and comment of the backup section. The
// host defaults to the machine's hostname, so machines sharing a repository
// and a source name keep apart: each one's parent and forget see only its
// own snapshots.
func (p *Profile) BackupOptions() []ark.BackupOption {
	b := p.Backup
	var opts []ark.BackupOption
	if len(b.Tags) > 0 {
		opts = append(opts, ark.WithTags(b.Tags...))
	}
	if len(b.Labels) > 0 {
		opts = append(opts, ark.WithLabels(b.Labels))
	}
	if h := p.host(); h != "" {
		opts = append(opts, ark.WithHost(h))
	}
	if b.Comment != "" {
		opts = append(opts, ark.WithComment(b.Comment))
	}
	return opts
}

// ForgetPolicy returns the keep-* rules of the forget section. Whether to
// prune afterwards is forget.prune, read by the caller.
func (p *Profile) ForgetPolicy() ark.ForgetPolicy {
	f := p.Forget
	policy := ark.ForgetPolicy{
		KeepLast:    f.KeepLast,
		KeepHourly:  f.KeepHourly,
		KeepDaily:   f.KeepDaily,
		KeepWeekly:  f.KeepWeekly,
		KeepMonthly: f.KeepMonthly,
		KeepYearly:  f.KeepYearly,
		KeepWithin:  f.KeepWithin,

		KeepWithinHourly:  f.KeepWithinHourly,
		KeepWithinDaily:   f.KeepWithinDaily,
		KeepWithinWeekly:  f.KeepWithinWeekly,
		KeepWithinMonthly: f.KeepWithinMonthly,
		KeepWithinYearly:  f.KeepWithinYearly,
	}
	for _, list := range f.KeepTags {
		policy.KeepTags = append(policy.KeepTags, ark.ParseTagList(list))
	}
	return policy
}

// Filter selects the snapshots the profile's backups store: source type
// docker-volume and the volume for backup.volume, else type file and the name
// [Profile.Source] gives, from the host [Profile.BackupOptions] records, so a
// forget through it leaves other sources, a directory and a volume of the
// same name, and other machines alone. It fails on a profile with none of
// backup.volume, backup.source and backup.path, which names no snapshots,
// rather than select every one. A volume profile without docker-host leaves
// the daemon to the caller; a caller that picks a remote one, from
// DOCKER_HOST say, sets the filter's Host to that machine itself.
func (p *Profile) Filter() (ark.SnapshotFilter, error) {
	source, err := p.sourceName()
	if err != nil {
		return ark.SnapshotFilter{}, err
	}
	if source == "" {
		return ark.SnapshotFilter{}, invalidf("no source name: set backup.path, backup.source or backup.volume")
	}
	typ := ark.SourceTypeFile
	if p.Backup.Volume != "" {
		typ = sourceTypeDockerVolume
	}
	return ark.SnapshotFilter{Source: source, Type: typ, Host: p.host()}, nil
}

// Open opens the repository at repo through [arkuri.Open] with pw, applying
// lock-wait and concurrency when set; opts come after them and win. The
// returned repository closes the backend on Close.
func (p *Profile) Open(ctx context.Context, pw ark.Password, opts ...ark.Option) (*ark.Repository, error) {
	if p.Repo == "" {
		return nil, invalidf("repo is empty: set it to a path, local:// or sftp:// location")
	}
	b, err := arkuri.Open(ctx, p.Repo)
	if err != nil {
		return nil, fmt.Errorf("open repository: %w", err)
	}
	var all []ark.Option
	if p.LockWait > 0 {
		all = append(all, ark.WithLockWait(p.LockWait))
	}
	if p.Concurrency > 0 {
		all = append(all, ark.WithConcurrency(p.Concurrency))
	}
	repo, err := ark.Open(ctx, b, pw, append(all, opts...)...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open repository: %w", err), b.Close())
	}
	return repo, nil
}
