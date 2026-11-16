package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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
   error, a file not named NAME.yaml as ignored; the command fails only when
   no profile loads.`,
				Action: profileList,
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

func profileList(_ context.Context, cmd *cli.Command) error {
	if cmd.NArg() != 0 {
		return exitcode.Usagef("ark profile list takes no arguments")
	}
	dir, err := profileDir()
	if err != nil {
		return err
	}
	des, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("list profiles: %w", err)
	}
	entries := []profileEntry{}
	files, loaded := 0, 0
	for _, de := range des {
		base := de.Name()
		name, ok := strings.CutSuffix(base, ".yaml")
		switch {
		case de.IsDir() || strings.HasPrefix(base, "."):
			continue
		case !ok:
			entries = append(entries, profileEntry{Name: base, Error: "ignored, -P reads NAME.yaml"})
			continue
		}
		files++
		p, err := arkprofile.Load(filepath.Join(dir, base))
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
	} else {
		p.Infof("no profiles in %s", dir)
	}
	if files > 0 && loaded == 0 {
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

// profileOnly is a value only the profile can set, else its default.
func profileOnly(key, value, def string) profileValue {
	if value == "" {
		return profileValue{key, def, "default"}
	}
	return profileValue{key, value, "profile"}
}

func profileInt(key string, n int) profileValue {
	if n == 0 {
		return profileValue{key, "0", "default"}
	}
	return profileValue{key, strconv.Itoa(n), "profile"}
}

func profileBool(key string, b bool) profileValue {
	if !b {
		return profileValue{key, "false", "default"}
	}
	return profileValue{key, "true", "profile"}
}

// effectiveValues resolves every value as the commands do: a flag given to
// ark, the profile, the environment, the default. The password only names its
// source.
func effectiveValues(root *cli.Command, lp *loadedProfile) []profileValue {
	host, _ := os.Hostname()
	b, f := lp.Backup, lp.Forget
	repo, repoFrom := repoSetting(root, lp)
	lockWait, lockWaitFrom := lockWaitSetting(root, lp)
	conc, concFrom := concurrencySetting(root, lp)
	values := []profileValue{
		{"repo", repo, repoFrom},
		passwordValue(root, lp.Profile),
		{"lock-wait", lockWait.String(), lockWaitFrom},
		{"concurrency", strconv.Itoa(conc), concFrom},
		profileOnly("backup.path", b.Path, ""),
		profileOnly("backup.tag", strings.Join(b.Tags, ", "), ""),
		profileOnly("backup.exclude", strings.Join(b.Excludes, ", "), ""),
		profileOnly("backup.exclude-file", strings.Join(b.ExcludeFiles, ", "), ""),
		profileBool("backup.exclude-cloud-files", b.ExcludeCloudFiles),
		profileOnly("backup.host", b.Host, host),
		profileOnly("backup.comment", b.Comment, ""),
		profileOnly("backup.source", b.Source, lp.Name),
		profileOnly("backup.label", labelList(b.Labels), ""),
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

// labelList shows labels as KEY="VALUE" pairs sorted by key; the value is
// quoted since it may hold commas.
func labelList(labels map[string]string) string {
	pairs := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		pairs = append(pairs, k+"="+strconv.Quote(labels[k]))
	}
	return strings.Join(pairs, ", ")
}

func hookValues(section string, h arkprofile.Hooks) []profileValue {
	return []profileValue{
		profileOnly(section+".run-before", h.RunBefore, ""),
		profileOnly(section+".run-after", h.RunAfter, ""),
		profileOnly(section+".run-after-fail", h.RunAfterFail, ""),
		profileOnly(section+".run-finally", h.RunFinally, ""),
	}
}

// passwordValue names the source repoPassword.read would use and never reads
// the password or a file. What would fail the command shows as the value.
func passwordValue(root *cli.Command, p *arkprofile.Profile) profileValue {
	insecure, from, err := repoPassword.resolve(root, *p)
	switch {
	case err != nil:
		return profileValue{"password", "error: " + err.Error(), "env"}
	case insecure.label != "" && from.label != "":
		return profileValue{"password", insecure.value + " conflicts with " + from.value, insecure.layer}
	case insecure.label != "":
		return profileValue{"password", insecure.value, insecure.layer}
	case from.label != "":
		return profileValue{"password", from.value, from.layer}
	}
	return profileValue{"password", "prompt", "default"}
}
