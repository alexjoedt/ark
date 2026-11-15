package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

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
	// fileEnv, commandEnv and insecureEnv are the variables behind the
	// flags, if any. commandFlag is optional.
	commandFlag, fileEnv, commandEnv, insecureEnv string
}

var (
	repoPassword = passwordSource{
		fileFlag:     "password-file",
		envVar:       "ARK_PASSWORD",
		insecureFlag: "insecure-no-password",
		prompt:       "repository password",
		commandFlag:  "password-command",
		fileEnv:      "ARK_PASSWORD_FILE",
		commandEnv:   "ARK_PASSWORD_COMMAND",
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
	label   string // names it in a conflict: "--password-file", "ARK_PASSWORD"
	layer   string // cli, profile or env
	value   string // what profile show prints: "file F", "command C", "variable V", "none, X"
	file    string // the password file the CLI reads itself, if any
	command string // the shell command whose output is the password, if any
}

// resolve picks the sources read would use without reading a password: the
// first setting for no password (flag, profile, variable) and the first
// password source (file or command flag, the profile's password-file or
// password-env, the file or command variable, the password variable). Either
// is zero when unset. A file and a command on the same layer conflict.
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

	flagFile, flagCmd := cmd.String(s.fileFlag), cmd.String(s.commandFlag)
	envFile, envCmd := os.Getenv(s.fileEnv), os.Getenv(s.commandEnv)
	switch {
	case flagFile != "" && flagCmd != "":
		return insecure, from, exitcode.Usagef("--%s conflicts with --%s", s.fileFlag, s.commandFlag)
	case flagFile != "":
		from = passwordOrigin{label: "--" + s.fileFlag, layer: "cli", value: "file " + flagFile, file: flagFile}
	case flagCmd != "":
		from = passwordOrigin{label: "--" + s.commandFlag, layer: "cli", value: "command " + flagCmd, command: flagCmd}
	case pp.PasswordFile != "":
		from = passwordOrigin{label: "the profile's password-file", layer: "profile", value: "file " + pp.PasswordFile}
	case pp.PasswordEnv != "":
		from = passwordOrigin{label: "the profile's password-env", layer: "profile", value: "variable " + pp.PasswordEnv}
	case envFile != "" && envCmd != "":
		return insecure, from, exitcode.Usagef("%s conflicts with %s", s.fileEnv, s.commandEnv)
	case envFile != "":
		from = passwordOrigin{label: s.fileEnv, layer: "env", value: "file " + envFile, file: envFile}
	case envCmd != "":
		from = passwordOrigin{label: s.commandEnv, layer: "env", value: "command " + envCmd, command: envCmd}
	case os.Getenv(s.envVar) != "":
		from = passwordOrigin{label: s.envVar, layer: "env", value: "variable " + s.envVar}
	}
	return insecure, from, nil
}

// read resolves the passphrase from the source resolve picks, else a prompt
// on the terminal. No password from the flag, the profile or the variable
// conflicts with every password source. confirm prompts twice.
func (s passwordSource) read(ctx context.Context, cmd *cli.Command, confirm bool, prof *arkprofile.Profile) (ark.Passphrase, error) {
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
	case from.command != "":
		pw, err := runPasswordCommand(ctx, from.command)
		if err != nil {
			return ark.Passphrase{}, err
		}
		return ark.WithPassword(pw), nil
	case from.label != "":
		return ark.WithPassword(os.Getenv(s.envVar)), nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		flags := "--" + s.fileFlag
		if s.commandFlag != "" {
			flags += ", --" + s.commandFlag
		}
		return ark.Passphrase{}, exitcode.Usagef(
			"no %s given: set %s or %s, or pass --%s for a repository without one",
			s.prompt,
			flags,
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

// runPasswordCommand runs command with sh -c and returns its stdout with one
// trailing newline stripped, as a password file is read. The command shares
// stdin and stderr, so a pinentry or a password manager can ask; its output
// never reaches an error message.
func runPasswordCommand(ctx context.Context, command string) (string, error) {
	c := exec.CommandContext(ctx, "sh", "-c", command)
	c.Stdin, c.Stderr = os.Stdin, os.Stderr
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("password command: %w", err)
	}
	pw := string(out)
	if s, ok := strings.CutSuffix(pw, "\n"); ok {
		pw = strings.TrimSuffix(s, "\r")
	}
	if pw == "" {
		return "", exitcode.Usagef("password command printed no password")
	}
	return pw, nil
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

					pw, err := newPassword.read(ctx, cmd, true, nil)
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
