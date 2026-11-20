package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/arkprofile"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/urfave/cli/v3"
)

type profileKey struct{}

// loadedProfile is the profile -P or -f named, with the directory its hooks
// run in.
type loadedProfile struct {
	*arkprofile.Profile
	dir string
}

// profileNamesSnapshots reports whether the profile names a source, so that
// snapshots and find narrow to it; forget refuses a profile that names none.
func profileNamesSnapshots(lp *loadedProfile) bool {
	return lp.Backup.Source != "" || lp.Backup.Path != "" || lp.Backup.Volume != ""
}

// profileScope narrows filter to the series the profile backs up, its source
// name and type, unless --source names another source; --type still wins.
// It returns the profile's filter.
func profileScope(cmd *cli.Command, lp *loadedProfile, filter *ark.SnapshotFilter) (ark.SnapshotFilter, error) {
	pf, err := lp.Filter()
	if err != nil {
		return pf, exitcode.Usagef("profile %s: %w", lp.Name, err)
	}
	if cmd.String("source") == "" {
		filter.Source = pf.Source
		if !cmd.IsSet("type") {
			filter.Type = pf.Type
		}
	}
	return pf, nil
}

// profileHost is the host the profile's backups are filed under. For a
// volume profile that is the host ark docker backup -P records, which is the
// daemon's own name for a remote $DOCKER_HOST without backup.host.
func profileHost(ctx context.Context, cmd *cli.Command, lp *loadedProfile, pf ark.SnapshotFilter) (string, error) {
	if lp.Backup.Volume == "" {
		return pf.Host, nil
	}
	host, err := dockerSeriesHost(ctx, cmd, func(ctx context.Context) (name string, err error) {
		c, err := arkdocker.NewClient(dockerHost(ctx, cmd))
		if err != nil {
			return "", err
		}
		defer func() { err = errors.Join(err, c.Close()) }()
		return c.DaemonName(ctx)
	})
	if err != nil {
		return "", fmt.Errorf("profile %s: %w; set backup.host to select the series without the daemon", lp.Name, err)
	}
	return host, nil
}

// profileFrom returns the profile loaded for this run, nil without one.
func profileFrom(ctx context.Context) *loadedProfile {
	lp, _ := ctx.Value(profileKey{}).(*loadedProfile)
	return lp
}

// loadProfile is the Before of every leaf command: the global flags given
// after the command name are parsed only by then. Every problem with the
// profile is a usage error naming the file.
func loadProfile(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	root := cmd.Root()
	name, file := root.String("profile"), root.String("profile-file")
	switch {
	case name != "" && file != "":
		return ctx, exitcode.Usagef("--profile and --profile-file exclude each other")
	case name == "" && file == "":
		return ctx, nil
	}
	if name != "" {
		var err error
		if file, err = profilePath(name); err != nil {
			return ctx, err
		}
	}
	lp, err := loadProfileFile(file)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, profileKey{}, lp), nil
}

// profilePath is the file of the profile NAME in the profile directory; a
// name that is not a valid profile name is a usage error.
func profilePath(name string) (string, error) {
	if err := (&arkprofile.Profile{Name: name}).Validate(); err != nil {
		return "", exitcode.Usagef("profile %w", err)
	}
	dir, err := profileDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".yaml"), nil
}

func loadProfileFile(file string) (*loadedProfile, error) {
	p, err := arkprofile.Load(file)
	if err != nil {
		return nil, exitcode.Usagef("%w", err)
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", file, err)
	}
	return &loadedProfile{p, filepath.Dir(abs)}, nil
}

// profileArg is the -P NAME or -f FILE the profile was given with, for a
// message that names the command to run instead.
func profileArg(cmd *cli.Command) string {
	if name := cmd.Root().String("profile"); name != "" {
		return "-P " + name
	}
	return "-f " + cmd.Root().String("profile-file")
}

// profileBackupCommand is the ark profile backup run of the profile -P or -f
// named, for a message that names the command to run instead.
func profileBackupCommand(cmd *cli.Command) string {
	if name := cmd.Root().String("profile"); name != "" {
		return "ark profile backup " + name
	}
	return "ark profile backup -f " + cmd.Root().String("profile-file")
}

// profileDir is $XDG_CONFIG_HOME/ark/profiles, else ~/.config/ark/profiles,
// on every OS.
func profileDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the profile directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "ark", "profiles"), nil
}

// setProfileLoad runs loadProfile before the own Before of every leaf command
// except version and profile list, which read no profile.
func setProfileLoad(cmd *cli.Command) {
	if len(cmd.Commands) == 0 && cmd.Name != "version" {
		own := cmd.Before
		cmd.Before = func(ctx context.Context, c *cli.Command) (context.Context, error) {
			ctx, err := loadProfile(ctx, c)
			if err != nil || own == nil {
				return ctx, err
			}
			return own(ctx, c)
		}
	}
	for _, sub := range cmd.Commands {
		if cmd.Name == "profile" && sub.Name == "list" {
			continue
		}
		setProfileLoad(sub)
	}
}

// withHooks runs fn between the hooks of the profile's section for command,
// backup or forget; without a profile it just runs fn. Hook output goes to
// stderr, stdout stays the command's.
func withHooks(ctx context.Context, lp *loadedProfile, command string, fn func(context.Context) error) error {
	if lp == nil {
		return fn(ctx)
	}
	hooks := lp.Backup.Hooks
	if command == "forget" {
		hooks = lp.Forget.Hooks
	}
	env := arkprofile.HookEnv{Dir: lp.dir, Profile: lp.Name, Command: command, Output: os.Stderr}
	return hooks.Run(ctx, env, fn)
}

// profilePassword reads the profile's password. A missing source or an
// empty file is a usage error, a file that cannot be read is not.
func profilePassword(p *arkprofile.Profile) (ark.Password, error) {
	pw, err := p.Password()
	var pe *os.PathError
	switch {
	case err == nil:
		return pw, nil
	case errors.As(err, &pe):
		return pw, fmt.Errorf("profile %s: %w", p.Name, err)
	}
	return pw, exitcode.Usagef("profile %s: %w", p.Name, err)
}
