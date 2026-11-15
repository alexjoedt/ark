package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	ark "github.com/alexjoedt/ark"
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
	case name != "":
		if err := (&arkprofile.Profile{Name: name}).Validate(); err != nil {
			return ctx, exitcode.Usagef("--profile: %w", err)
		}
		dir, err := profileDir()
		if err != nil {
			return ctx, err
		}
		file = filepath.Join(dir, name+".yaml")
	case file == "":
		return ctx, nil
	}
	p, err := arkprofile.Load(file)
	if err != nil {
		return ctx, exitcode.Usagef("%w", err)
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return ctx, fmt.Errorf("profile %s: %w", file, err)
	}
	return context.WithValue(ctx, profileKey{}, &loadedProfile{p, filepath.Dir(abs)}), nil
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

// setProfileLoad gives every leaf command loadProfile as its Before.
func setProfileLoad(cmd *cli.Command) {
	if len(cmd.Commands) == 0 && cmd.Before == nil {
		cmd.Before = loadProfile
	}
	for _, sub := range cmd.Commands {
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

// profilePassphrase reads the profile's password. A missing source or an
// empty file is a usage error, a file that cannot be read is not.
func profilePassphrase(p *arkprofile.Profile) (ark.Passphrase, error) {
	pw, err := p.Passphrase()
	var pe *os.PathError
	switch {
	case err == nil:
		return pw, nil
	case errors.As(err, &pe):
		return pw, fmt.Errorf("profile %s: %w", p.Name, err)
	}
	return pw, exitcode.Usagef("profile %s: %w", p.Name, err)
}
