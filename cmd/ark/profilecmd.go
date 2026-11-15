package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/alexjoedt/ark/arkprofile"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/urfave/cli/v3"
)

func profileCommand() *cli.Command {
	return &cli.Command{
		Name:  "profile",
		Usage: "inspect profiles",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list the profiles in the profile directory",
				UsageText: `ark profile list

   Prints name, repository and backup path of every profile in
   $XDG_CONFIG_HOME/ark/profiles. A file that does not load is listed with its
   error; the command fails only when no profile loads.`,
				Action: func(_ context.Context, cmd *cli.Command) error { return profileList(cmd) },
			},
			{
				Name:      "show",
				Usage:     "print the effective values of a profile and where each comes from",
				ArgsUsage: "[NAME]",
				UsageText: `ark profile show NAME
   ark profile show -f FILE

   Prints every value a command would use with the profile: the global flags
   given here (cli), the profile, the environment and the defaults. The
   password is never printed, only where it comes from.`,
				Action: profileShow,
			},
		},
	}
}

type profileEntry struct {
	Name  string `json:"name"`
	Repo  string `json:"repo"`
	Path  string `json:"path"`
	Error string `json:"error,omitempty"`
}

func profileList(cmd *cli.Command) error {
	if cmd.NArg() != 0 {
		return exitcode.Usagef("ark profile list takes no arguments")
	}
	dir, err := profileDir()
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return fmt.Errorf("list profiles: %w", err)
	}
	slices.Sort(files)
	entries := []profileEntry{}
	loaded := 0
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		p, err := arkprofile.Load(f)
		if err != nil {
			entries = append(entries, profileEntry{Name: name, Error: strings.ReplaceAll(err.Error(), "\n", " ")})
			continue
		}
		loaded++
		entries = append(entries, profileEntry{Name: p.Name, Repo: p.Repo, Path: p.Backup.Path})
	}

	p := printer(cmd)
	if p.Mode().JSON {
		if err := p.JSON(entries); err != nil {
			return err
		}
	} else if len(entries) > 0 {
		rows := make([][]string, len(entries))
		for i, e := range entries {
			rows[i] = []string{e.Name, e.Repo, e.Path, e.Error}
		}
		p.Table([]string{"NAME", "REPO", "BACKUP PATH", "ERROR"}, rows)
	}
	if len(files) > 0 && loaded == 0 {
		err := errors.New("no profile loads")
		if p.Mode().JSON {
			return &exitcode.Error{Code: exitcode.Failure, Kind: exitcode.KindError, Err: err, Silent: true}
		}
		return err
	}
	return nil
}

type profileValue struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

func profileShow(ctx context.Context, cmd *cli.Command) error {
	lp := profileFrom(ctx)
	switch {
	case cmd.NArg() > 1:
		return exitcode.Usagef("usage: ark profile show NAME, or -f FILE")
	case cmd.NArg() == 1 && lp != nil:
		return exitcode.Usagef("give the profile as NAME or with -P or -f, not both")
	case cmd.NArg() == 1:
		file, err := profilePath(cmd.Args().First())
		if err != nil {
			return err
		}
		if lp, err = loadProfileFile(file); err != nil {
			return err
		}
	case lp == nil:
		return exitcode.Usagef("usage: ark profile show NAME, or -f FILE")
	}

	values := effectiveValues(cmd.Root(), lp)
	p := printer(cmd)
	if p.Mode().JSON {
		return p.JSON(map[string]any{"profile": lp.Name, "values": values})
	}
	rows := make([][]string, len(values))
	for i, v := range values {
		rows[i] = []string{v.Key, v.Value, v.Source}
	}
	p.Table([]string{"KEY", "VALUE", "SOURCE"}, rows)
	return nil
}

// candidate is one layer a value may come from.
type candidate struct {
	source, value string
	ok            bool
}

// layered is the first candidate that is set; the last one is the default.
func layered(key string, cs ...candidate) profileValue {
	for _, c := range cs {
		if c.ok {
			return profileValue{key, c.value, c.source}
		}
	}
	return profileValue{key, "", "default"}
}

func fromProfile(value string, set bool) candidate { return candidate{"profile", value, set} }

// profileOnly is a value only the profile can set, else its default.
func profileOnly(key, value, def string) profileValue {
	return layered(key, fromProfile(value, value != ""), candidate{"default", def, true})
}

func profileInt(key string, n int) profileValue {
	return layered(key, fromProfile(strconv.Itoa(n), n != 0), candidate{"default", "0", true})
}

func profileBool(key string, b bool) profileValue {
	return layered(key, fromProfile("true", b), candidate{"default", "false", true})
}

// effectiveValues mirrors the precedence of the commands: a flag given to
// ark, the profile, the environment, the default. The password only names its
// source.
func effectiveValues(root *cli.Command, lp *loadedProfile) []profileValue {
	env := os.Getenv("ARK_REPOSITORY")
	host, _ := os.Hostname()
	b, f := lp.Backup, lp.Forget
	values := []profileValue{
		layered("repo",
			candidate{"cli", root.String("repo"), root.IsSet("repo")},
			fromProfile(lp.Repo, lp.Repo != ""),
			candidate{"env", env, env != ""}),
		passwordValue(root, lp.Profile),
		layered("lock-wait",
			candidate{"cli", root.Duration("lock-wait").String(), root.IsSet("lock-wait")},
			fromProfile(lp.LockWait.String(), lp.LockWait > 0),
			candidate{"default", root.Duration("lock-wait").String(), true}),
		layered("concurrency",
			candidate{"cli", strconv.Itoa(root.Int("concurrency")), root.IsSet("concurrency")},
			fromProfile(strconv.Itoa(lp.Concurrency), lp.Concurrency > 0),
			candidate{"default", strconv.Itoa(root.Int("concurrency")), true}),
		profileOnly("backup.path", b.Path, ""),
		profileOnly("backup.tag", strings.Join(b.Tags, ", "), ""),
		profileOnly("backup.exclude", strings.Join(b.Excludes, ", "), ""),
		profileOnly("backup.exclude-file", strings.Join(b.ExcludeFiles, ", "), ""),
		profileBool("backup.exclude-cloud-files", b.ExcludeCloudFiles),
		profileOnly("backup.host", b.Host, host),
		profileOnly("backup.comment", b.Comment, ""),
		profileOnly("backup.name", b.Name, lp.Name),
	}
	values = append(values, hookValues("backup", b.Hooks)...)
	values = append(values,
		profileInt("forget.keep-last", f.KeepLast),
		profileInt("forget.keep-hourly", f.KeepHourly),
		profileInt("forget.keep-daily", f.KeepDaily),
		profileInt("forget.keep-weekly", f.KeepWeekly),
		profileInt("forget.keep-monthly", f.KeepMonthly),
		profileInt("forget.keep-yearly", f.KeepYearly),
		profileOnly("forget.keep-within", f.KeepWithin, ""),
		profileBool("forget.prune", f.Prune),
	)
	return append(values, hookValues("forget", f.Hooks)...)
}

func hookValues(section string, h arkprofile.Hooks) []profileValue {
	return []profileValue{
		profileOnly(section+".run-before", h.RunBefore, ""),
		profileOnly(section+".run-after", h.RunAfter, ""),
		profileOnly(section+".run-after-fail", h.RunAfterFail, ""),
		profileOnly(section+".run-finally", h.RunFinally, ""),
	}
}

// passwordValue names the source repoPassword.read would use, in its order,
// and never reads the password or a file.
func passwordValue(root *cli.Command, p *arkprofile.Profile) profileValue {
	src := repoPassword
	var insecure, pw profileValue
	insecureEnv, _ := strconv.ParseBool(os.Getenv(src.insecureEnv))
	switch {
	case root.Bool(src.insecureFlag):
		insecure = profileValue{"password", "none, --" + src.insecureFlag, "cli"}
	case p.InsecureNoPassword:
		insecure = profileValue{"password", "none, insecure-no-password", "profile"}
	case insecureEnv:
		insecure = profileValue{"password", "none, " + src.insecureEnv, "env"}
	}
	switch envFile, envPw := os.Getenv(src.fileEnv), os.Getenv(src.envVar); {
	case root.String(src.fileFlag) != "":
		pw = profileValue{"password", "file " + root.String(src.fileFlag), "cli"}
	case p.PasswordFile != "":
		pw = profileValue{"password", "file " + p.PasswordFile, "profile"}
	case p.PasswordEnv != "":
		pw = profileValue{"password", "variable " + p.PasswordEnv, "profile"}
	case envFile != "":
		pw = profileValue{"password", "file " + envFile, "env"}
	case envPw != "":
		pw = profileValue{"password", "variable " + src.envVar, "env"}
	default:
		pw = profileValue{"password", "prompt", "default"}
	}
	switch {
	case insecure.Source != "" && pw.Source != "default":
		insecure.Value += " conflicts with " + pw.Value
		return insecure
	case insecure.Source != "":
		return insecure
	}
	return pw
}
