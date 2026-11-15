package arkprofile

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkuri"
)

// snapshotName is the source name of the profile's snapshots: backup.name,
// else the profile's name, else the absolute backup.path as arkfs names it.
func (p *Profile) snapshotName() string {
	switch {
	case p.Backup.Name != "":
		return p.Backup.Name
	case p.Name != "", p.Backup.Path == "":
		return p.Name
	}
	abs, err := filepath.Abs(p.Backup.Path)
	if err != nil {
		return p.Backup.Path
	}
	return abs
}

// Source returns the arkfs source of backup.path with the profile's excludes,
// exclude files and cloud-file option, named backup.name, else the profile's
// name.
func (p *Profile) Source() (ark.Source, error) {
	b := p.Backup
	if b.Path == "" {
		return nil, invalidf("backup.path is empty")
	}
	opts := []arkfs.SourceOption{arkfs.WithName(p.snapshotName())}
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

// BackupOptions returns the tags, host and comment of the backup section. An
// empty host is left out, so the snapshot records none unless the caller adds
// [ark.WithHost] after these options.
func (p *Profile) BackupOptions() []ark.BackupOption {
	b := p.Backup
	var opts []ark.BackupOption
	if len(b.Tags) > 0 {
		opts = append(opts, ark.WithTags(b.Tags...))
	}
	if b.Host != "" {
		opts = append(opts, ark.WithHost(b.Host))
	}
	if b.Comment != "" {
		opts = append(opts, ark.WithComment(b.Comment))
	}
	return opts
}

// ForgetPolicy returns the keep-* rules of the forget section. It fails only
// on a keep-within that is not a positive duration, which [Profile.Validate]
// reports too. Whether to prune afterwards is forget.prune, read by the caller.
func (p *Profile) ForgetPolicy() (ark.ForgetPolicy, error) {
	f := p.Forget
	policy := ark.ForgetPolicy{
		KeepLast:    f.KeepLast,
		KeepHourly:  f.KeepHourly,
		KeepDaily:   f.KeepDaily,
		KeepWeekly:  f.KeepWeekly,
		KeepMonthly: f.KeepMonthly,
		KeepYearly:  f.KeepYearly,
	}
	if f.KeepWithin != "" {
		d, err := parseWithin(f.KeepWithin)
		if err != nil {
			return ark.ForgetPolicy{}, invalidf("forget.keep-within: %v", err)
		}
		policy.KeepWithin = d
	}
	return policy, nil
}

// Filter selects the snapshots [Profile.Source] names, so a forget through it
// leaves the snapshots of other sources alone. A profile without a name and
// without backup.path gives the zero filter, which selects every snapshot.
func (p *Profile) Filter() ark.SnapshotFilter {
	return ark.SnapshotFilter{Name: p.snapshotName()}
}

// Open opens the repository at repo through [arkuri.Open] with pw, applying
// lock-wait and concurrency when set; opts come after them and win. The
// returned repository closes the backend on Close.
func (p *Profile) Open(ctx context.Context, pw ark.Passphrase, opts ...ark.Option) (*ark.Repository, error) {
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
