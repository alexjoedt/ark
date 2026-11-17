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

// sourceName is the source name of the profile's snapshots: backup.source,
// else the profile's name, else the absolute backup.path as arkfs names it.
func (p *Profile) sourceName() string {
	switch {
	case p.Backup.Source != "":
		return p.Backup.Source
	case p.Name != "", p.Backup.Path == "":
		return p.Name
	}
	abs, err := filepath.Abs(p.Backup.Path)
	if err != nil {
		return p.Backup.Path
	}
	return abs
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
// exclude files and cloud-file option, named backup.source, else the profile's
// name, else the absolute backup.path.
func (p *Profile) Source() (ark.Source, error) {
	b := p.Backup
	if b.Path == "" {
		return nil, invalidf("backup.path is empty")
	}
	opts := []arkfs.SourceOption{arkfs.WithName(p.sourceName())}
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
// and a profile name keep apart: each one's parent and forget see only its
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
	for _, list := range f.KeepTags {
		policy.KeepTags = append(policy.KeepTags, ark.ParseTagList(list))
	}
	return policy, nil
}

// Filter selects the snapshots [Profile.Source] names from the host
// [Profile.BackupOptions] records, so a forget through it leaves other sources
// and other machines alone. It fails on a profile without a name and without
// backup.path, which names no snapshots, rather than select every one.
func (p *Profile) Filter() (ark.SnapshotFilter, error) {
	source := p.sourceName()
	if source == "" {
		return ark.SnapshotFilter{}, invalidf("no source name: set the profile's name, backup.source or backup.path")
	}
	return ark.SnapshotFilter{Source: source, Host: p.host()}, nil
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
