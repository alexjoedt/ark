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
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkprofile"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/urfave/cli/v3"
)

func profileCommand() *cli.Command {
	return &cli.Command{
		Name:  "profile",
		Usage: "inspect profiles and run their backup",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list the profiles in the profile directory",
				UsageText: `ark profile list

   Prints name, repository and backup path, or volume NAME, of every profile
   in $XDG_CONFIG_HOME/ark/profiles. A file that does not load is listed with its
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
			{
				Name:      "backup",
				Usage:     "run the backup a profile describes, of its directory or its docker volume",
				ArgsUsage: "[NAME]",
				UsageText: `ark profile backup NAME
   ark profile backup -f FILE

   Backs up what the profile names, with every value from the profile:
   backup.path as ark backup -P NAME does, backup.volume as ark docker
   backup -P NAME does, stopping the containers that use it unless
   backup.no-stop is set. Takes no backup flags; to override a value give it
   to ark backup -P or ark docker backup -P.`,
				Action: profileBackupAction,
			},
		},
	}
}

type profileEntry struct {
	Name   string `json:"name"`
	Repo   string `json:"repo"`
	Path   string `json:"path,omitempty"`
	Volume string `json:"volume,omitempty"`
	Error  string `json:"error,omitempty"`
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
		entries = append(entries, profileEntry{
			Name: p.Name, Repo: p.Repo, Path: p.Backup.Path, Volume: p.Backup.Volume,
		})
	}

	p := printer(cmd)
	if p.Mode().JSON {
		if err := p.JSON(entries); err != nil {
			return err
		}
	} else if len(entries) > 0 {
		rows := make([][]string, len(entries))
		for i, e := range entries {
			backup := e.Path
			if e.Volume != "" {
				backup = "volume " + e.Volume
			}
			rows[i] = []string{e.Name, e.Repo, backup, e.Error}
		}
		p.Table([]string{"NAME", "REPO", "BACKUP", "ERROR"}, rows)
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

// profileOperand is the profile a profile subcommand names, as NAME or with
// -P or -f.
func profileOperand(ctx context.Context, cmd *cli.Command) (*loadedProfile, error) {
	lp := profileFrom(ctx)
	usage := "usage: ark profile " + cmd.Name + " NAME, or -f FILE"
	switch {
	case cmd.NArg() > 1:
		return nil, exitcode.Usagef("%s", usage)
	case cmd.NArg() == 1 && lp != nil:
		return nil, exitcode.Usagef("give the profile as NAME or with -P or -f, not both")
	case cmd.NArg() == 1:
		file, err := profilePath(cmd.Args().First())
		if err != nil {
			return nil, err
		}
		return loadProfileFile(file)
	case lp == nil:
		return nil, exitcode.Usagef("%s", usage)
	}
	return lp, nil
}

// profileBackupAction backs up the profile's backup.path or backup.volume.
func profileBackupAction(ctx context.Context, cmd *cli.Command) error {
	lp, err := profileOperand(ctx, cmd)
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, profileKey{}, lp)
	switch {
	case lp.Backup.Volume != "":
		return runVolumeBackup(ctx, cmd, lp, lp.Backup.Volume, lp.Backup)
	case lp.Backup.Path != "":
		return runFileBackup(ctx, cmd, lp, *lp.Profile, nil, nil)
	}
	return exitcode.Usagef("profile %s names no backup.path or backup.volume to back up", lp.Name)
}

func profileShow(ctx context.Context, cmd *cli.Command) error {
	lp, err := profileOperand(ctx, cmd)
	if err != nil {
		return err
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

// quotedList shows each tag list of forget.keep-tag in quotes, so the commas
// inside a list and a list of the empty tag stay readable.
func quotedList(lists []string) string {
	quoted := make([]string, len(lists))
	for i, l := range lists {
		quoted[i] = strconv.Quote(l)
	}
	return strings.Join(quoted, ", ")
}

func profileCount(key string, n ark.KeepCount) profileValue {
	if n == 0 {
		return profileValue{key, "0", "default"}
	}
	return profileValue{key, n.String(), "profile"}
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
	var source string
	if pf, err := lp.Filter(); err == nil && b.Volume == "" {
		source = pf.Source
	}
	repo, repoFrom := repoSetting(root, lp)
	lockWait, lockWaitFrom := lockWaitSetting(root, lp)
	conc, concFrom := concurrencySetting(root, lp)
	values := []profileValue{
		{"repo", repo, repoFrom},
		passwordValue(root, lp.Profile),
		{"lock-wait", lockWait.String(), lockWaitFrom},
		{"concurrency", strconv.Itoa(conc), concFrom},
		profileOnly("docker-host", lp.DockerHost, ""),
		profileOnly("backup.path", b.Path, ""),
		profileOnly("backup.volume", b.Volume, ""),
		profileOnly("backup.tag", strings.Join(b.Tags, ", "), ""),
		profileOnly("backup.exclude", strings.Join(b.Excludes, ", "), ""),
		profileOnly("backup.exclude-file", strings.Join(b.ExcludeFiles, ", "), ""),
		profileBool("backup.exclude-cloud-files", b.ExcludeCloudFiles),
		profileOnly("backup.host", b.Host, host),
		profileOnly("backup.comment", b.Comment, ""),
		profileOnly("backup.source", b.Source, source),
		profileOnly("backup.label", labelList(b.Labels), ""),
		profileBool("backup.no-stop", b.NoStop),
		stopTimeoutValue(b.StopTimeout),
	}
	values = append(values, hookValues("backup", b.Hooks)...)
	values = append(values,
		profileCount("forget.keep-last", f.KeepLast),
		profileCount("forget.keep-hourly", f.KeepHourly),
		profileCount("forget.keep-daily", f.KeepDaily),
		profileCount("forget.keep-weekly", f.KeepWeekly),
		profileCount("forget.keep-monthly", f.KeepMonthly),
		profileCount("forget.keep-yearly", f.KeepYearly),
		profileOnly("forget.keep-within", f.KeepWithin.String(), ""),
		profileOnly("forget.keep-within-hourly", f.KeepWithinHourly.String(), ""),
		profileOnly("forget.keep-within-daily", f.KeepWithinDaily.String(), ""),
		profileOnly("forget.keep-within-weekly", f.KeepWithinWeekly.String(), ""),
		profileOnly("forget.keep-within-monthly", f.KeepWithinMonthly.String(), ""),
		profileOnly("forget.keep-within-yearly", f.KeepWithinYearly.String(), ""),
		profileOnly("forget.keep-tag", quotedList(f.KeepTags), ""),
		profileBool("forget.prune", f.Prune),
	)
	return append(values, hookValues("forget", f.Hooks)...)
}

// stopTimeoutValue is backup.stop-timeout, 0 showing the default.
func stopTimeoutValue(d time.Duration) profileValue {
	if d == 0 {
		return profileValue{"backup.stop-timeout", defaultStopTimeout.String(), "default"}
	}
	return profileValue{"backup.stop-timeout", d.String(), "profile"}
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
