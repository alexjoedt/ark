package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkprofile"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// passwordSource names the flags and the variables one password is read from.
// There is no --password flag, it would show in the process list.
type passwordSource struct {
	fileFlag, envVar, insecureFlag, prompt string
	// fileEnv and insecureEnv are the variables behind the two flags, if any.
	fileEnv, insecureEnv string
}

var (
	repoPassword = passwordSource{
		fileFlag:     "password-file",
		envVar:       "ARK_PASSWORD",
		insecureFlag: "insecure-no-password",
		prompt:       "repository password",
		fileEnv:      "ARK_PASSWORD_FILE",
		insecureEnv:  "ARK_INSECURE_NO_PASSWORD",
	}
	newPassword = passwordSource{
		fileFlag:     "new-password-file",
		envVar:       "ARK_NEW_PASSWORD",
		insecureFlag: "new-insecure-no-password",
		prompt:       "new repository password",
	}
)

// passwordOrigin is the setting a password, or no password, comes from.
type passwordOrigin struct {
	label string // names it in a conflict: "--password-file", "ARK_PASSWORD"
	layer string // cli, profile or env
	value string // what profile show prints: "file F", "variable V", "none, X"
	file  string // the password file the CLI reads itself, if any
}

// resolve picks the sources read would use without reading a password: the
// first setting for no password (flag, profile, variable) and the first
// password source (file flag, the profile's password-file or password-env,
// the file variable, the password variable). Either is zero when unset.
func (s passwordSource) resolve(cmd *cli.Command, pp arkprofile.Profile) (insecure, from passwordOrigin, err error) {
	switch v := os.Getenv(s.insecureEnv); {
	case cmd.Bool(s.insecureFlag):
		insecure = passwordOrigin{label: "--" + s.insecureFlag, layer: "cli"}
	case pp.InsecureNoPassword:
		insecure = passwordOrigin{label: "the profile's insecure-no-password", layer: "profile"}
	case v != "":
		on, err := strconv.ParseBool(v)
		if err != nil {
			return insecure, from, exitcode.Usagef("%s=%q: want true or false", s.insecureEnv, v)
		}
		if on {
			insecure = passwordOrigin{label: s.insecureEnv, layer: "env"}
		}
	}
	if insecure.label != "" {
		insecure.value = "none, " + insecure.label
	}

	switch flagFile, envFile := cmd.String(s.fileFlag), os.Getenv(s.fileEnv); {
	case flagFile != "":
		from = passwordOrigin{"--" + s.fileFlag, "cli", "file " + flagFile, flagFile}
	case pp.PasswordFile != "":
		from = passwordOrigin{"the profile's password-file", "profile", "file " + pp.PasswordFile, ""}
	case pp.PasswordEnv != "":
		from = passwordOrigin{"the profile's password-env", "profile", "variable " + pp.PasswordEnv, ""}
	case envFile != "":
		from = passwordOrigin{s.fileEnv, "env", "file " + envFile, envFile}
	case os.Getenv(s.envVar) != "":
		from = passwordOrigin{s.envVar, "env", "variable " + s.envVar, ""}
	}
	return insecure, from, nil
}

// read resolves the passphrase from the source resolve picks, else a prompt
// on the terminal. No password from the flag, the profile or the variable
// conflicts with every password source. confirm prompts twice.
func (s passwordSource) read(cmd *cli.Command, confirm bool, prof *arkprofile.Profile) (ark.Passphrase, error) {
	var pp arkprofile.Profile
	if prof != nil {
		pp = *prof
	}
	insecure, from, err := s.resolve(cmd, pp)
	if err != nil {
		return ark.Passphrase{}, err
	}
	if insecure.label != "" && from.label != "" {
		return ark.Passphrase{}, exitcode.Usagef("%s conflicts with %s", insecure.label, from.label)
	}

	switch {
	case insecure.label != "":
		return ark.WithoutPassword(), nil
	case from.layer == "profile":
		return profilePassphrase(prof)
	case from.file != "":
		pw, err := readPasswordFile(from.file)
		if err != nil {
			return ark.Passphrase{}, err
		}
		return ark.WithPassword(pw), nil
	case from.label != "":
		return ark.WithPassword(os.Getenv(s.envVar)), nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return ark.Passphrase{}, exitcode.Usagef(
			"no %s given: set --%s or %s, or pass --%s for a repository without one",
			s.prompt,
			s.fileFlag,
			s.envVar,
			s.insecureFlag,
		)
	}
	pw, err := promptPassword(fd, "Enter "+s.prompt+": ")
	if err != nil {
		return ark.Passphrase{}, err
	}
	if confirm {
		again, err := promptPassword(fd, "Repeat "+s.prompt+": ")
		if err != nil {
			return ark.Passphrase{}, err
		}
		if again != pw {
			return ark.Passphrase{}, exitcode.Usagef("passwords do not match")
		}
	}
	return ark.WithPassword(pw), nil
}

// readPasswordFile reads the file as a profile's password-file is read; an
// empty file is a usage error.
func readPasswordFile(path string) (string, error) {
	pw, err := arkprofile.ReadPasswordFile(path)
	if errors.Is(err, arkprofile.ErrNoPassword) {
		return "", exitcode.Usagef("password file %s is empty", path)
	}
	return pw, err
}

func promptPassword(fd int, prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if len(b) == 0 {
		return "", exitcode.Usagef("empty password")
	}
	return string(b), nil
}

func keyCommand() *cli.Command {
	return &cli.Command{
		Name:  "key",
		Usage: "manage the repository password",
		Commands: []*cli.Command{
			{
				Name:  "passwd",
				Usage: "change the repository password",
				UsageText: `ark key passwd [--new-password-file FILE | --new-insecure-no-password]

   Opens the repository with the current password, then stores the master key
   under the new one. The new password comes from --new-password-file, then
   ARK_NEW_PASSWORD, then a prompt asked twice. The master key stays the same:
   whoever could read it before still can.`,
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "new-password-file", Usage: "read the new password from `FILE`"},
					&cli.BoolFlag{
						Name:  "new-insecure-no-password",
						Usage: "remove the password, anyone with backend access can read the repository",
					},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					repo, err := openRepo(ctx, cmd)
					if err != nil {
						return err
					}
					defer repo.Close()

					pw, err := newPassword.read(cmd, true, nil)
					if err != nil {
						return err
					}
					if err := repo.ChangePassword(ctx, pw); err != nil {
						return fmt.Errorf("change password: %w", err)
					}

					protected := !cmd.Bool("new-insecure-no-password")
					p := printer(cmd)
					if p.Mode().JSON {
						return p.JSON(map[string]any{"password": protected})
					}
					if protected {
						p.Linef("password changed")
					} else {
						p.Linef("password removed, the repository is no longer protected")
					}
					return nil
				},
			},
		},
	}
}
