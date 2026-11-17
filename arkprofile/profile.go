// Package arkprofile reads and checks profiles: stored backup configurations
// of one repository, one directory and one forget policy, written by hand as
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
//	  source: documents              # source name, default: the profile's name
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
//	  keep-within: 30d               # a duration such as 72h or 30d
//	  keep-tag: [pinned, "db,prod"]  # keep what is tagged pinned, or db and prod
//	  prune: true                    # prune after the forget
//	  run-after: echo forgotten      # the same four hooks as backup
//
// $VAR and ${VAR} expand in every value but the hooks. In repo (unless it is a
// URI), password-file, backup.path and backup.exclude-file, ~ is the home
// directory and a relative path is taken relative to the profile's directory.
// A profile never holds a password, only where to find one.
//
// [Profile.Open], [Profile.Source], [Profile.BackupOptions],
// [Profile.ForgetPolicy] and [Profile.Filter] translate a profile onto the
// ark API; running backup, forget and prune stays with the caller.
package arkprofile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexjoedt/ark"
)

var (
	// ErrInvalid is matched by every error that rejects the contents of a
	// profile: a decode error, a bad name or conflicting password sources.
	ErrInvalid = errors.New("arkprofile: invalid profile")

	// ErrNoPassword is returned by [Profile.Passphrase] when the profile names
	// no password source, so the caller can fall back to its own.
	ErrNoPassword = errors.New("arkprofile: no password source")
)

// Profile is one stored backup configuration.
type Profile struct {
	// Name is the profile's name, set by [Load] from the file's base name. It
	// defaults the source name and the source filter of forget.
	Name string `yaml:"-"`

	Repo               string        `yaml:"repo"                 ark:"location"`
	PasswordFile       string        `yaml:"password-file"        ark:"path"`
	PasswordEnv        string        `yaml:"password-env"`
	InsecureNoPassword bool          `yaml:"insecure-no-password"`
	LockWait           time.Duration `yaml:"lock-wait"`
	Concurrency        int           `yaml:"concurrency"`

	Backup Backup `yaml:"backup"`
	Forget Forget `yaml:"forget"`
}

// Backup holds the flags of a backup.
type Backup struct {
	Path              string   `yaml:"path"                ark:"path"`
	Tags              []string `yaml:"tag"`
	Excludes          []string `yaml:"exclude"`
	ExcludeFiles      []string `yaml:"exclude-file"        ark:"path"`
	ExcludeCloudFiles bool     `yaml:"exclude-cloud-files"`
	Host              string   `yaml:"host"`
	Comment           string   `yaml:"comment"`
	// Source is the snapshot's source name; empty means the profile's name.
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
	// KeepWithin is a duration as the CLI takes it, such as 72h or 30d.
	KeepWithin string `yaml:"keep-within"`
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
// [ark.ValidateLabels] refuses, a keep-within that is not a positive
// duration and a keep-tag list with an empty tag next to others, as a stray
// comma leaves one. Every error matches [ErrInvalid].
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
	if f.KeepWithin != "" {
		if _, err := parseWithin(f.KeepWithin); err != nil {
			return invalidf("forget.keep-within: %v", err)
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

// parseWithin reads keep-within as the CLI does: a Go duration or a day
// count such as 30d, days being 24 hours, and positive.
func parseWithin(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	var err error
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n float64
		n, err = strconv.ParseFloat(days, 64)
		d = time.Duration(n * float64(24*time.Hour))
	} else {
		d, err = time.ParseDuration(s)
	}
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 72h or 30d", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
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

// Passphrase reads the password from password-file, else from the variable
// password-env names. insecure-no-password gives a repository without one.
// A profile without any of them returns [ErrNoPassword].
func (p *Profile) Passphrase() (ark.Passphrase, error) {
	if err := p.checkPassword(); err != nil {
		return ark.Passphrase{}, err
	}
	switch {
	case p.InsecureNoPassword:
		return ark.WithoutPassword(), nil
	case p.PasswordFile != "":
		pw, err := ReadPasswordFile(p.PasswordFile)
		if err != nil {
			return ark.Passphrase{}, err
		}
		return ark.WithPassword(pw), nil
	case p.PasswordEnv != "":
		pw := os.Getenv(p.PasswordEnv)
		if pw == "" {
			return ark.Passphrase{}, fmt.Errorf("password-env: variable %s is not set or empty", p.PasswordEnv)
		}
		return ark.WithPassword(pw), nil
	}
	return ark.Passphrase{}, ErrNoPassword
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
