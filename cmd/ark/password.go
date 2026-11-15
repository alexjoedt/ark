package main

import (
	"cmp"
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

// read resolves the passphrase from the first source that is set: the
// password file flag, the profile's password-file or password-env, the file
// variable, the password variable, else a prompt on the terminal. No password
// from the flag, the profile or the variable conflicts with every password
// source. confirm prompts twice.
func (s passwordSource) read(cmd *cli.Command, confirm bool, prof *arkprofile.Profile) (ark.Passphrase, error) {
	var pp arkprofile.Profile
	if prof != nil {
		pp = *prof
	}
	var insecure string
	switch v := getenv(s.insecureEnv); {
	case cmd.Bool(s.insecureFlag):
		insecure = "--" + s.insecureFlag
	case pp.InsecureNoPassword:
		insecure = "the profile's insecure-no-password"
	case v != "":
		on, err := strconv.ParseBool(v)
		if err != nil {
			return ark.Passphrase{}, exitcode.Usagef("%s=%q: want true or false", s.insecureEnv, v)
		}
		if on {
			insecure = s.insecureEnv
		}
	}

	flagFile, envFile, env := cmd.String(s.fileFlag), getenv(s.fileEnv), os.Getenv(s.envVar)
	var from string
	switch {
	case flagFile != "":
		from = "--" + s.fileFlag
	case pp.PasswordFile != "":
		from = "the profile's password-file"
	case pp.PasswordEnv != "":
		from = "the profile's password-env"
	case envFile != "":
		from = s.fileEnv
	case env != "":
		from = s.envVar
	}
	if insecure != "" && from != "" {
		return ark.Passphrase{}, exitcode.Usagef("%s conflicts with %s", insecure, from)
	}

	switch {
	case insecure != "":
		return ark.WithoutPassword(), nil
	case flagFile == "" && (pp.PasswordFile != "" || pp.PasswordEnv != ""):
		return profilePassphrase(prof)
	case flagFile != "" || envFile != "":
		pw, err := readPasswordFile(cmp.Or(flagFile, envFile))
		if err != nil {
			return ark.Passphrase{}, err
		}
		return ark.WithPassword(pw), nil
	case env != "":
		return ark.WithPassword(env), nil
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

// getenv is [os.Getenv] that reads nothing for an empty name.
func getenv(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
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
