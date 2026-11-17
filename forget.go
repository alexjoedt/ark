package ark

import (
	"fmt"
	"strconv"
	"time"
)

// KeepCount is the count of a counted keep rule: zero keeps nothing,
// [Unlimited] keeps without a limit. Forget refuses any other negative count
// with [ErrInvalidPolicy]. As text, in a profile or on the command line, a
// count is a whole number or the word unlimited.
type KeepCount int

// Unlimited makes a counted rule keep all it can: KeepLast every snapshot,
// a period rule the newest snapshot of every period.
const Unlimited KeepCount = -1

// ParseKeepCount reads a whole number of zero or more, or the word
// unlimited. A negative number is refused, so a typo cannot keep everything.
func ParseKeepCount(s string) (KeepCount, error) {
	if s == "unlimited" {
		return Unlimited, nil
	}
	n, err := strconv.Atoi(s)
	switch {
	case err != nil:
		return 0, fmt.Errorf("%q is not a whole number or unlimited", s)
	case n < 0:
		return 0, fmt.Errorf("negative count %d: write unlimited to keep all", n)
	}
	return KeepCount(n), nil
}

// String returns the count as ParseKeepCount reads it.
func (c KeepCount) String() string {
	if c == Unlimited {
		return "unlimited"
	}
	return strconv.Itoa(int(c))
}

// MarshalText implements [encoding.TextMarshaler].
func (c KeepCount) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler] with ParseKeepCount.
func (c *KeepCount) UnmarshalText(text []byte) error {
	n, err := ParseKeepCount(string(text))
	if err != nil {
		return err
	}
	*c = n
	return nil
}

// ForgetPolicy defines the retention rules applied by Repository.Forget.
// The policy is applied per series: the snapshots that share source type,
// source name and host. A busy series never pushes another's snapshots out of
// the window, and two hosts backing up the same source each keep their own.
// Zero value keeps everything.
//
// A counted rule, KeepLast to KeepYearly, takes a number or [Unlimited].
//
// The hours, days, weeks, months and years of KeepHourly to KeepYearly are
// those of the local time zone, not of UTC; see WithForgetLocation.
type ForgetPolicy struct {
	// KeepLast keeps the most recent n snapshots regardless of age.
	KeepLast KeepCount

	// KeepHourly keeps one snapshot per hour for the most recent n hours.
	KeepHourly KeepCount

	// KeepDaily keeps one snapshot per day for the most recent n days.
	KeepDaily KeepCount

	// KeepWeekly keeps one snapshot per ISO week for the most recent n weeks.
	KeepWeekly KeepCount

	// KeepMonthly keeps one snapshot per month for the most recent n months.
	KeepMonthly KeepCount

	// KeepYearly keeps one snapshot per year for the most recent n years.
	KeepYearly KeepCount

	// KeepWithin keeps every snapshot younger than this, measured from the
	// newest snapshot of the group rather than from the wall clock, so a
	// repository that stopped receiving backups does not lose everything.
	KeepWithin time.Duration
}

// counted lists the counted rules of p by field name.
func (p ForgetPolicy) counted() [6]struct {
	name string
	n    KeepCount
} {
	return [6]struct {
		name string
		n    KeepCount
	}{
		{"KeepLast", p.KeepLast},
		{"KeepHourly", p.KeepHourly},
		{"KeepDaily", p.KeepDaily},
		{"KeepWeekly", p.KeepWeekly},
		{"KeepMonthly", p.KeepMonthly},
		{"KeepYearly", p.KeepYearly},
	}
}

// validate refuses a negative count other than Unlimited.
func (p ForgetPolicy) validate() error {
	for _, c := range p.counted() {
		if c.n < Unlimited {
			return fmt.Errorf("%w: %s is %d, want 0 or more, or Unlimited", ErrInvalidPolicy, c.name, int(c.n))
		}
	}
	return nil
}

// keepsNothing reports whether no rule of a valid p keeps a snapshot. An
// unlimited count is a rule; a negative window keeps nothing, the same as a
// zero one.
func (p ForgetPolicy) keepsNothing() bool {
	for _, c := range p.counted() {
		if c.n != 0 {
			return false
		}
	}
	return p.KeepWithin <= 0
}

// ForgetResult is returned by Repository.Forget.
type ForgetResult struct {
	Keep   []*Snapshot
	Remove []*Snapshot
	DryRun bool

	// KeepReasons names, per kept snapshot ID, every rule that selected it,
	// in the order the rules are applied and in restic's wording, such as
	// "within 30d", "last snapshot" or "daily snapshot". A snapshot that is
	// not kept has no entry; when nothing is kept the map may be nil or empty.
	KeepReasons map[string][]string
}

// forgetConfig holds configuration for a Forget call.
type forgetConfig struct {
	dryRun bool
	loc    *time.Location
}

// ForgetOption configures a Forget call.
type ForgetOption func(*forgetConfig)

// WithForgetDryRun makes Forget report what would be removed without deleting anything.
func WithForgetDryRun() ForgetOption { return func(c *forgetConfig) { c.dryRun = true } }

// WithForgetLocation sets the time zone the period rules (KeepHourly to
// KeepYearly) count in: a snapshot belongs to the hour, day, week, month or
// year its time falls in on a clock in loc. Without it, and for a nil loc,
// Forget counts in [time.Local], so the same policy can keep different
// snapshots on machines in different zones; a program that must not depend on
// the machine passes a fixed zone such as [time.UTC]. KeepLast and KeepWithin
// do not depend on it.
func WithForgetLocation(loc *time.Location) ForgetOption {
	return func(c *forgetConfig) {
		if loc != nil {
			c.loc = loc
		}
	}
}
