package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// passwordSource names the flags and the variable one password is read from.
// There is no --password flag, it would show in the process list.
type passwordSource struct {
	fileFlag, envVar, insecureFlag, prompt string
}

var (
	repoPassword = passwordSource{"password-file", "ARK_PASSWORD", "insecure-no-password", "repository password"}
	newPassword  = passwordSource{
		"new-password-file",
		"ARK_NEW_PASSWORD",
		"new-insecure-no-password",
		"new repository password",
	}
)

// read resolves the passphrase from the flags of cmd: the password file, then
// the environment, then a prompt on the terminal. confirm prompts twice.
func (s passwordSource) read(cmd *cli.Command, confirm bool) (ark.Passphrase, error) {
	file := cmd.String(s.fileFlag)
	env := os.Getenv(s.envVar)
	if cmd.Bool(s.insecureFlag) {
		if file != "" || env != "" {
			return ark.Passphrase{}, exitcode.Usagef(
				"--%s conflicts with --%s and %s",
				s.insecureFlag,
				s.fileFlag,
				s.envVar,
			)
		}
		return ark.WithoutPassword(), nil
	}

	switch {
	case file != "":
		pw, err := readPasswordFile(file)
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

// readPasswordFile returns the file's contents without one trailing line
// ending, so both `echo pw > file` and an editor's save work.
func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	pw := string(data)
	if s, ok := strings.CutSuffix(pw, "\n"); ok {
		pw = strings.TrimSuffix(s, "\r")
	}
	if pw == "" {
		return "", exitcode.Usagef("password file %s is empty", path)
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

					pw, err := newPassword.read(cmd, true)
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
