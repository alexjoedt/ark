// Package arkprofile reads and checks profiles: stored backup configurations
// of one repository, one directory and one forget policy, written by hand as
// one YAML file per profile. The CLI fills the flags of its commands from a
// profile; an embedder such as a scheduler builds a Profile in code or embeds
// it in its own configuration struct and decodes that with [Decode].
//
// The keys of a profile file are the names of the CLI flags:
//
//	repo: sftp://backup@nas/srv/ark
//	password-file: ~/.config/ark/docs.pw
//	backup:
//	  path: ~/Documents
//	  tag: [docs]
//	  exclude: ["*.tmp"]
//	forget:
//	  keep-daily: 7
//	  prune: true
//
// A profile never holds a password, only where to find one.
package arkprofile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
	// defaults the snapshot name and the name filter of forget.
	Name string `yaml:"-"`

	Repo               string        `yaml:"repo"`
	PasswordFile       string        `yaml:"password-file"`
	PasswordEnv        string        `yaml:"password-env"`
	InsecureNoPassword bool          `yaml:"insecure-no-password"`
	LockWait           time.Duration `yaml:"lock-wait"`
	Concurrency        int           `yaml:"concurrency"`

	Backup Backup `yaml:"backup"`
	Forget Forget `yaml:"forget"`
}

// Backup holds the flags of a backup.
type Backup struct {
	Path              string   `yaml:"path"`
	Tags              []string `yaml:"tag"`
	Excludes          []string `yaml:"exclude"`
	ExcludeFiles      []string `yaml:"exclude-file"`
	ExcludeCloudFiles bool     `yaml:"exclude-cloud-files"`
	Host              string   `yaml:"host"`
	Comment           string   `yaml:"comment"`
	// Name is the snapshot's source name; empty means the profile's name.
	Name string `yaml:"name"`

	Hooks `yaml:",inline"`
}

// Forget holds the policy of a forget.
type Forget struct {
	KeepLast    int `yaml:"keep-last"`
	KeepHourly  int `yaml:"keep-hourly"`
	KeepDaily   int `yaml:"keep-daily"`
	KeepWeekly  int `yaml:"keep-weekly"`
	KeepMonthly int `yaml:"keep-monthly"`
	KeepYearly  int `yaml:"keep-yearly"`
	// KeepWithin is a duration as the CLI takes it, such as 72h or 30d.
	KeepWithin string `yaml:"keep-within"`
	Prune      bool   `yaml:"prune"`

	Hooks `yaml:",inline"`
}

// Hooks are shell commands run around a command, each through sh -c.
type Hooks struct {
	RunBefore    string `yaml:"run-before"`
	RunAfter     string `yaml:"run-after"`
	RunAfterFail string `yaml:"run-after-fail"`
	RunFinally   string `yaml:"run-finally"`
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Validate reports a name outside [a-z0-9][a-z0-9._-]*, conflicting
// password sources, a negative number or lock-wait and a keep-within that is
// not a positive duration. Every error matches [ErrInvalid].
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
	f := p.Forget
	for _, n := range []struct {
		key string
		v   int
	}{
		{"concurrency", p.Concurrency},
		{"forget.keep-last", f.KeepLast},
		{"forget.keep-hourly", f.KeepHourly},
		{"forget.keep-daily", f.KeepDaily},
		{"forget.keep-weekly", f.KeepWeekly},
		{"forget.keep-monthly", f.KeepMonthly},
		{"forget.keep-yearly", f.KeepYearly},
	} {
		if n.v < 0 {
			return invalidf("%s %d: want 0 or more", n.key, n.v)
		}
	}
	if f.KeepWithin != "" {
		if _, err := parseWithin(f.KeepWithin); err != nil {
			return invalidf("forget.keep-within: %v", err)
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
// refuses, decodes it with [Decode], names the profile after the file's base
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
	var p Profile
	if err := Decode(f, &p); err != nil {
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

type invalidError struct{ msg string }

func (e *invalidError) Error() string        { return e.msg }
func (e *invalidError) Is(target error) bool { return target == ErrInvalid }

func invalidf(format string, args ...any) error {
	return &invalidError{fmt.Sprintf(format, args...)}
}
