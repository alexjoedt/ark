// Package arkprofile reads and checks profiles: stored backup configurations
// of one repository, one directory or docker volume and one forget policy, written by hand as
// one YAML file per profile. The CLI fills the flags of its commands from a
// profile; an embedder such as a scheduler builds a Profile in code or embeds
// it in its own configuration struct and decodes that with [Decode].
//
// The keys of a profile file are the names of the CLI flags. A file with
// every key, all of them optional:
//
//	repo: sftp://backup@nas/srv/ark  # a path, local:// or sftp:// location
//	password-file: ~/.config/ark/docs.pw  # or password-env: VAR, or insecure-no-password: true
//	lock-wait: 30s                   # retry a held lock this long
//	concurrency: 4                   # backend operations at once
//	backup:
//	  path: ~/Documents              # the directory to back up
//	  tag: [docs, daily]
//	  exclude: ["*.tmp", .cache]
//	  exclude-file: [docs.exclude]   # patterns one per line, relative to this file
//	  exclude-cloud-files: true      # leave out files that exist only in the cloud
//	  host: laptop                   # host recorded in the snapshot
//	  comment: nightly
//	  source: documents              # source name, default: the absolute path
//	  label: {app: docs}             # labels recorded in the snapshot
//	  run-before: mount /mnt/docs    # hooks, each run through sh -c
//	  run-after: echo stored
//	  run-after-fail: 'echo "backup failed: $ARK_ERROR"'
//	  run-finally: umount /mnt/docs
//	forget:
//	  keep-last: 3
//	  keep-hourly: 24
//	  keep-daily: 7
//	  keep-weekly: 4
//	  keep-monthly: 12
//	  keep-yearly: unlimited         # a count, or unlimited for every period
//	  keep-within: 30d               # years, months, days, hours: 72h, 1y6m
//	  keep-within-daily: 14d         # the newest of every day in the window,
//	  keep-within-monthly: 1y        # also -hourly, -weekly and -yearly
//	  keep-tag: [pinned, "db,prod"]  # keep what is tagged pinned, or db and prod
//	  prune: true                    # prune after the forget
//	  run-after: echo forgotten      # the same four hooks as backup
//
// A volume profile backs up a docker volume instead of a directory. It takes
// the backup keys above but path, exclude, exclude-file, exclude-cloud-files
// and source; the volume names the snapshots:
//
//	repo: /srv/ark
//	docker-host: ssh://admin@db1     # the daemon, default: DOCKER_HOST, else the local one
//	backup:
//	  volume: pgdata                 # the docker volume to back up
//	  host: db1                      # needed with a remote docker-host
//	  tag: [db]
//
// $VAR and ${VAR} expand in every value but the hooks. In repo (unless it is a
// URI), password-file, backup.path and backup.exclude-file, ~ is the home
// directory and a relative path is taken relative to the profile's directory.
// A profile never holds a password, only where to find one.
//
// [Profile.Open], [Profile.Source], [Profile.BackupOptions],
// [Profile.ForgetPolicy] and [Profile.Filter] translate a profile onto the
// ark API; running backup, forget and prune stays with the caller, and so
// does the source of a volume profile: arkprofile does not import the Docker
// SDK, the caller builds it with arkdocker from backup.volume and docker-host.
package arkprofile

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alexjoedt/ark"
)

var (
	// ErrInvalid is matched by every error that rejects the contents of a
	// profile: a decode error, a bad name or conflicting password sources.
	ErrInvalid = errors.New("arkprofile: invalid profile")

	// ErrNoPassword is returned by [Profile.Password] when the profile names
	// no password source, so the caller can fall back to its own.
	ErrNoPassword = errors.New("arkprofile: no password source")
)

// Profile is one stored backup configuration.
type Profile struct {
	// Name is the profile's name, set by [Load] from the file's base name. It
	// names the profile in messages and hooks; the snapshots' source name
	// comes from backup.source or backup.path, never from Name.
	Name string `yaml:"-"`

	Repo               string        `yaml:"repo"                 ark:"location"`
	PasswordFile       string        `yaml:"password-file"        ark:"path"`
	PasswordEnv        string        `yaml:"password-env"`
	InsecureNoPassword bool          `yaml:"insecure-no-password"`
	LockWait           time.Duration `yaml:"lock-wait"`
	Concurrency        int           `yaml:"concurrency"`
	// DockerHost is the docker daemon of a volume profile, as DOCKER_HOST
	// takes it; empty leaves the choice to the caller.
	DockerHost string `yaml:"docker-host"`

	Backup Backup `yaml:"backup"`
	Forget Forget `yaml:"forget"`
}

// Backup holds the flags of a backup.
type Backup struct {
	Path string `yaml:"path" ark:"path"`
	// Volume is the docker volume to back up instead of Path. [Decode] and
	// Validate refuse it beside the keys its conflicts tag lists.
	Volume string `yaml:"volume" conflicts:"path exclude exclude-file exclude-cloud-files source"`

	Tags              []string `yaml:"tag"`
	Excludes          []string `yaml:"exclude"`
	ExcludeFiles      []string `yaml:"exclude-file"        ark:"path"`
	ExcludeCloudFiles bool     `yaml:"exclude-cloud-files"`
	Host              string   `yaml:"host"`
	Comment           string   `yaml:"comment"`
	// Source is the snapshot's source name; empty means the absolute Path,
	// the name a plain backup of that path stores.
	Source string `yaml:"source"`
	// Labels are recorded in the snapshot; their keys follow [ark.ValidateLabels].
	Labels map[string]string `yaml:"label"`

	Hooks `yaml:",inline"`
}

// Forget holds the policy of a forget. A keep count is a whole number or
// the word unlimited, in code [ark.Unlimited].
type Forget struct {
	KeepLast    ark.KeepCount `yaml:"keep-last"`
	KeepHourly  ark.KeepCount `yaml:"keep-hourly"`
	KeepDaily   ark.KeepCount `yaml:"keep-daily"`
	KeepWeekly  ark.KeepCount `yaml:"keep-weekly"`
	KeepMonthly ark.KeepCount `yaml:"keep-monthly"`
	KeepYearly  ark.KeepCount `yaml:"keep-yearly"`
	// KeepWithin is written as [ark.ParseWithin] reads it, such as 30d, 72h
	// or 1y6m; m counts months.
	KeepWithin ark.Within `yaml:"keep-within"`
	// KeepWithinHourly to KeepWithinYearly are windows in the same form.
	KeepWithinHourly  ark.Within `yaml:"keep-within-hourly"`
	KeepWithinDaily   ark.Within `yaml:"keep-within-daily"`
	KeepWithinWeekly  ark.Within `yaml:"keep-within-weekly"`
	KeepWithinMonthly ark.Within `yaml:"keep-within-monthly"`
	KeepWithinYearly  ark.Within `yaml:"keep-within-yearly"`
	// KeepTags holds one tag list per entry, each as the CLI takes it and
	// [ark.ParseTagList] reads it: "db,prod" keeps what carries both tags,
	// "" what carries none.
	KeepTags []string `yaml:"keep-tag"`
	Prune    bool     `yaml:"prune"`

	Hooks `yaml:",inline"`
}

// Hooks are shell commands run around a command, each through sh -c.
type Hooks struct {
	RunBefore    string `yaml:"run-before"     ark:"raw"`
	RunAfter     string `yaml:"run-after"      ark:"raw"`
	RunAfterFail string `yaml:"run-after-fail" ark:"raw"`
	RunFinally   string `yaml:"run-finally"    ark:"raw"`
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Validate reports a name outside [a-z0-9][a-z0-9._-]*, conflicting
// password sources, a negative concurrency or lock-wait, a negative keep
// count other than [ark.Unlimited], a backup label that
// [ark.ValidateLabels] refuses, a keep-within window that [ark.ParseWithin]
// refuses and a keep-tag list with an empty tag next to others, as a stray
// comma leaves one. It refuses backup.volume beside a set backup.path,
// exclude key or backup.source, as [Decode] does, and a volume profile on a
// remote docker-host (not unix:// or npipe://) without backup.host, since the
// snapshots record the daemon's machine. Every error matches [ErrInvalid].
func (p *Profile) Validate() error {
	if !validName.MatchString(p.Name) {
		return invalidf(
			"name %q: use lower-case letters, digits, '.', '_' and '-', starting with a letter or digit",
			p.Name,
		)
	}
	if err := p.checkPassword(); err != nil {
		return err
	}
	if p.LockWait < 0 {
		return invalidf("lock-wait %s: want 0 or more", p.LockWait)
	}
	if err := p.checkVolume(); err != nil {
		return err
	}
	if err := ark.ValidateLabels(p.Backup.Labels); err != nil {
		return invalidf("backup.label: %w", err)
	}
	if p.Concurrency < 0 {
		return invalidf("concurrency %d: want 0 or more", p.Concurrency)
	}
	f := p.Forget
	for _, n := range []struct {
		key string
		v   ark.KeepCount
	}{
		{"forget.keep-last", f.KeepLast},
		{"forget.keep-hourly", f.KeepHourly},
		{"forget.keep-daily", f.KeepDaily},
		{"forget.keep-weekly", f.KeepWeekly},
		{"forget.keep-monthly", f.KeepMonthly},
		{"forget.keep-yearly", f.KeepYearly},
	} {
		if n.v < ark.Unlimited {
			return invalidf("%s %d: want 0 or more, or unlimited", n.key, int(n.v))
		}
	}
	// The keys are parsed when the profile is read; a window set in code
	// has to be one the key can hold.
	for _, w := range []struct {
		key string
		v   ark.Within
	}{
		{"forget.keep-within", f.KeepWithin},
		{"forget.keep-within-hourly", f.KeepWithinHourly},
		{"forget.keep-within-daily", f.KeepWithinDaily},
		{"forget.keep-within-weekly", f.KeepWithinWeekly},
		{"forget.keep-within-monthly", f.KeepWithinMonthly},
		{"forget.keep-within-yearly", f.KeepWithinYearly},
	} {
		if w.v == (ark.Within{}) {
			continue
		}
		if _, err := ark.ParseWithin(w.v.String()); err != nil {
			return invalidf("%s: %v", w.key, err)
		}
	}
	for _, list := range f.KeepTags {
		if tags := ark.ParseTagList(list); len(tags) > 1 && slices.Contains(tags, "") {
			return invalidf(
				`forget.keep-tag %q: an empty tag next to others, drop the stray comma; "" alone keeps the untagged`,
				list,
			)
		}
	}
	return nil
}

func (p *Profile) checkVolume() error {
	for _, sec := range []struct {
		prefix string
		v      reflect.Value
	}{{"", reflect.ValueOf(*p)}, {"backup.", reflect.ValueOf(p.Backup)}, {"forget.", reflect.ValueOf(p.Forget)}} {
		fields, err := structFields(sec.v)
		if err != nil {
			return err
		}
		if k, c, ok := conflict(fields, slices.Sorted(maps.Keys(fields))); ok {
			return invalidf("%s%s and %s%s exclude each other", sec.prefix, k, sec.prefix, c)
		}
	}
	if p.Backup.Volume != "" && remoteDockerHost(p.DockerHost) && p.Backup.Host == "" {
		return invalidf("docker-host %s is a remote daemon: set backup.host to the name of its machine, "+
			"the host its snapshots record", p.DockerHost)
	}
	return nil
}

// remoteDockerHost reports whether the daemon address is on another machine:
// set and neither a unix socket nor a windows pipe, as the CLI tells it.
func remoteDockerHost(addr string) bool {
	return addr != "" && !strings.HasPrefix(addr, "unix://") && !strings.HasPrefix(addr, "npipe://")
}

func (p *Profile) checkPassword() error {
	if p.PasswordFile != "" && p.PasswordEnv != "" {
		return invalidf("password-file and password-env exclude each other")
	}
	if p.InsecureNoPassword && (p.PasswordFile != "" || p.PasswordEnv != "") {
		return invalidf("insecure-no-password conflicts with password-file and password-env")
	}
	return nil
}

// Password reads the password from password-file, else from the variable
// password-env names. insecure-no-password gives a repository without one.
// A profile without any of them returns [ErrNoPassword].
func (p *Profile) Password() (ark.Password, error) {
	if err := p.checkPassword(); err != nil {
		return ark.Password{}, err
	}
	switch {
	case p.InsecureNoPassword:
		return ark.NoPassword(), nil
	case p.PasswordFile != "":
		pw, err := ReadPasswordFile(p.PasswordFile)
		if err != nil {
			return ark.Password{}, err
		}
		return ark.NewPassword(pw), nil
	case p.PasswordEnv != "":
		pw := os.Getenv(p.PasswordEnv)
		if pw == "" {
			return ark.Password{}, fmt.Errorf("password-env: variable %s is not set or empty", p.PasswordEnv)
		}
		return ark.NewPassword(pw), nil
	}
	return ark.Password{}, ErrNoPassword
}

// ReadPasswordFile returns the file's contents without one trailing line
// ending, so both `echo pw > file` and an editor's save work. An empty
// password is an error matching [ErrNoPassword].
func ReadPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	pw := string(data)
	if s, ok := strings.CutSuffix(pw, "\n"); ok {
		pw = strings.TrimSuffix(s, "\r")
	}
	if pw == "" {
		return "", fmt.Errorf("password file %s is empty: %w", path, ErrNoPassword)
	}
	return pw, nil
}

// Load reads the profile file at path. It refuses a file that [CheckFile]
// refuses, decodes it with [Decode] relative to the file's directory, names the profile after the file's base
// name without its extension and validates it.
func Load(path string) (*Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open profile: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("open profile: %w", err)
	}
	if err := CheckFile(fi); err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	var p Profile
	if err := Decode(f, &p, dir); err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	base := filepath.Base(path)
	p.Name = strings.TrimSuffix(base, filepath.Ext(base))
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	return &p, nil
}

// CheckFile refuses a profile file that someone else could have written: one
// writable by group or others, or owned by a user other than the caller and
// root. A service that reads its own file through [Decode] passes the
// file's Stat result.
func CheckFile(fi fs.FileInfo) error {
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("writable by group or others (mode %04o): run chmod go-w on it", perm)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot tell the file's owner")
	}
	if uid := int(st.Uid); uid != 0 && uid != os.Getuid() {
		return fmt.Errorf("owned by uid %d, neither you nor root", uid)
	}
	return nil
}

type invalidError struct{ err error }

func (e *invalidError) Error() string        { return e.err.Error() }
func (e *invalidError) Is(target error) bool { return target == ErrInvalid }
func (e *invalidError) Unwrap() error        { return e.err }

func invalidf(format string, args ...any) error {
	return &invalidError{fmt.Errorf(format, args...)}
}
