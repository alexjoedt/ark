package ark

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// KeepCount is the count of a counted keep rule: zero keeps nothing,
// [Unlimited] keeps without a limit. Forget refuses any other negative count
// with [ErrInvalidPolicy]. As text, in a profile or on the command line, a
// count is a whole number or the word unlimited. In JSON it is a number, and
// the string "unlimited" for [Unlimited].
type KeepCount int

// Unlimited makes a counted rule keep all it can: KeepLast every snapshot,
// a period rule the newest snapshot of every period.
const Unlimited KeepCount = -1

// ParseKeepCount reads a whole number of zero or more, or the word
// unlimited in any case. A negative number is refused, so a typo cannot keep
// everything.
func ParseKeepCount(s string) (KeepCount, error) {
	if strings.EqualFold(s, "unlimited") {
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

// MarshalJSON writes the count as a number and [Unlimited] as the string
// "unlimited", so a policy stored as JSON keeps plain numbers.
func (c KeepCount) MarshalJSON() ([]byte, error) {
	if c == Unlimited {
		return []byte(`"unlimited"`), nil
	}
	return []byte(strconv.Itoa(int(c))), nil
}

// UnmarshalJSON reads a number, or a string in the form ParseKeepCount reads.
func (c *KeepCount) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	return c.UnmarshalText(bytes.Trim(data, `"`))
}

// Within is the window of a keep-within rule, in restic's calendar units: a
// number of years, months and days, which are steps on the calendar, and of
// hours, which are elapsed time. A month back from 15 March is 15 February,
// however long February is; a step onto a day the month lacks runs over into
// the next, so a month back from 31 March is 3 March, or 2 March in a leap
// year. The zero value is no window. Forget refuses a negative number, more
// than 11 months and any number above a million with [ErrInvalidPolicy]; 18
// months are a year and 6 months. These are the bounds of [ParseWithin], so
// String writes every window Forget takes in a form ParseWithin reads.
//
// As text, in a profile, on the command line and in JSON, a window is that
// form, such as "30d"; the zero value is the empty text.
type Within struct {
	Years, Months, Days, Hours int
}

// maxWithin bounds the years, days and hours of a Within, so the start of
// the window stays inside what [time.Time] and [time.Duration] hold.
const maxWithin = 1_000_000

// maxWithinMonths bounds the months: twelve are a year, and in the text
// form a larger number is more likely meant as minutes.
const maxWithinMonths = 11

var withinText = regexp.MustCompile(`^(?:(\d+)y)?(?:(\d+)m)?(?:(\d+)d)?(?:(\d+)h)?$`)

// ParseWithin reads a window from its text form, the one a profile and the
// command line take: whole numbers followed by y (years), m (months), d
// (days) or h (hours), each unit at most once and in that order, such as 30d,
// 72h or 1y6m. These are restic's units, read more strictly: restic takes
// them in any order and takes any number of months. Here m goes up to 11,
// because a larger number is more likely meant as minutes, which do not
// exist; 18 months are 1y6m. A window of no length is refused.
func ParseWithin(s string) (Within, error) {
	const form = "write whole numbers of y (years), m (months), d (days) and h (hours) in that order, " +
		"as in 30d, 72h or 1y6m"
	s = strings.TrimSpace(s)
	m := withinText.FindStringSubmatch(s)
	if m == nil {
		return Within{}, fmt.Errorf("%q is not a duration: %s", s, form)
	}
	var n [4]int
	for i := range n {
		if m[i+1] == "" {
			continue
		}
		var err error
		if n[i], err = strconv.Atoi(m[i+1]); err != nil {
			return Within{}, fmt.Errorf("%q: a number above %d: %s", s, maxWithin, form)
		}
	}
	w := Within{Years: n[0], Months: n[1], Days: n[2], Hours: n[3]}
	switch {
	case w == Within{}:
		return Within{}, fmt.Errorf("duration %q is zero: %s", s, form)
	case w.Months > maxWithinMonths:
		return Within{}, fmt.Errorf("%q: m counts months, up to %d, there are no minutes: %s", s, maxWithinMonths, form)
	}
	if err := w.validate(); err != nil {
		return Within{}, fmt.Errorf("%q: %w: %s", s, err, form)
	}
	return w, nil
}

// withinPart is one number of a Within: its value, field name, unit and the
// largest value Forget takes.
type withinPart struct {
	n     int
	name  string
	unit  string
	limit int
}

// parts lists the numbers of w from years to hours.
func (w Within) parts() [4]withinPart {
	return [4]withinPart{
		{w.Years, "Years", "y", maxWithin},
		{w.Months, "Months", "m", maxWithinMonths},
		{w.Days, "Days", "d", maxWithin},
		{w.Hours, "Hours", "h", maxWithin},
	}
}

// String returns the window as restic writes it, from years to hours with
// the zero parts left out, such as "1y6m" or "72h". No unit is turned into
// another: 72 hours are not written as 3 days, which can be a different span
// of time. The zero value is the empty string.
func (w Within) String() string {
	var b strings.Builder
	for _, part := range w.parts() {
		if part.n != 0 {
			b.WriteString(strconv.Itoa(part.n))
			b.WriteString(part.unit)
		}
	}
	return b.String()
}

// MarshalText implements [encoding.TextMarshaler] with String.
func (w Within) MarshalText() ([]byte, error) { return []byte(w.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler] with ParseWithin. The
// empty text is the zero value, no window.
func (w *Within) UnmarshalText(text []byte) error {
	*w = Within{}
	if len(bytes.TrimSpace(text)) == 0 {
		return nil
	}
	parsed, err := ParseWithin(string(text))
	if err != nil {
		return err
	}
	*w = parsed
	return nil
}

func (w Within) validate() error {
	for _, part := range w.parts() {
		if part.n < 0 || part.n > part.limit {
			return fmt.Errorf("%s is %d, want 0 to %d", part.name, part.n, part.limit)
		}
	}
	return nil
}

// before returns the start of the window that ends at t. Years, months and
// days are stepped on the calendar of t's location, hours are elapsed time.
// A step onto a time the clocks of the location skip, at the start of summer
// time, lands an hour before or after it, as [time.Time.AddDate] decides.
func (w Within) before(t time.Time) time.Time {
	return t.AddDate(-w.Years, -w.Months, -w.Days).Add(-time.Duration(w.Hours) * time.Hour)
}

// ForgetPolicy defines the retention rules applied by Repository.Forget.
// The policy is applied per series: the snapshots that share source type,
// source name and host. A busy series never pushes another's snapshots out of
// the window, and two hosts backing up the same source each keep their own.
// Zero value keeps everything.
//
// A counted rule, KeepLast to KeepYearly, takes a number or [Unlimited].
//
// A period rule, KeepHourly to KeepYearly, keeps the newest snapshot of each
// period, newest period first, until its count is used up. When it reaches
// the oldest snapshot of the series with count left, it keeps that one too,
// although its period is taken, so a series shorter than the policy is kept
// from its first snapshot on; the reason then reads "oldest daily snapshot".
// This is restic's rule.
//
// The hours, days, weeks, months and years of KeepHourly to KeepYearly and
// of KeepWithinHourly to KeepWithinYearly are those of the local time zone,
// not of UTC; see WithForgetLocation.
//
// A snapshot dated after the time of the run, as a machine with a wrong
// clock leaves it, is kept and seen by no rule: it uses no count, takes the
// place of no snapshot in a period, moves no window and is not the oldest
// snapshot of its series, and a tag list does not select it. Its one reason
// is "in the future", and [ForgetResult.Future] lists it. Here ark leaves
// restic, which lets such a snapshot take the first count and the newest
// slot of its period, and a real snapshot is removed in its place. To remove
// one, name its ID in the filter with a policy that has no rule.
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

	// KeepWithin keeps every snapshot younger than this window, measured
	// from the newest snapshot of the series rather than from the wall
	// clock, so a repository that stopped receiving backups does not lose
	// everything. A snapshot exactly the window old is not kept. A snapshot
	// dated in the future is no part of this: the window is measured from
	// the newest snapshot that is not, and the future ones are kept as
	// [ForgetPolicy] says, not by this rule. The years, months and days are
	// counted in the zone of WithForgetLocation.
	KeepWithin Within

	// KeepWithinHourly keeps the newest snapshot of every hour inside this
	// window. It is KeepHourly bounded by a time instead of a count, on the
	// window of KeepWithin: a snapshot exactly the window old or older is
	// never kept, and the oldest snapshot of the series is kept in addition
	// when it lies inside, for the reason "oldest hourly within 30d". This
	// is restic's rule, and so are the four that follow.
	KeepWithinHourly Within

	// KeepWithinDaily keeps the newest snapshot of every day inside this
	// window, as KeepWithinHourly does for hours.
	KeepWithinDaily Within

	// KeepWithinWeekly keeps the newest snapshot of every ISO week inside
	// this window.
	KeepWithinWeekly Within

	// KeepWithinMonthly keeps the newest snapshot of every month inside
	// this window.
	KeepWithinMonthly Within

	// KeepWithinYearly keeps the newest snapshot of every year inside this
	// window.
	KeepWithinYearly Within

	// KeepTags keeps every snapshot that carries all tags of at least one
	// of its lists: {{"a", "b"}, {"c"}} keeps what is tagged a and b, and
	// what is tagged c. The empty tag stands for no tag, so {{""}} keeps the
	// untagged snapshots. Blanks around a tag do not count, in the list or
	// on the snapshot. The rule has no count and takes none from the
	// others: a snapshot it keeps still uses a KeepLast slot. A list
	// without a tag, and one with the empty tag next to others, which no
	// snapshot could match, are refused with [ErrInvalidPolicy].
	//
	// Every other rule keeps at least the newest snapshot of a series that
	// is not dated in the future, this one may keep none. Forget then
	// removes nothing at all and returns [ErrNoSnapshotKept]; a snapshot
	// kept for being in the future does not count as kept here.
	KeepTags [][]string
}

// ParseTagList reads one list of KeepTags from its text form, the one a
// profile and the command line take: tags separated by commas, the blanks
// around a tag dropped. The empty text is the list of the empty tag, which
// matches untagged snapshots. This is restic's --keep-tag syntax. A stray
// comma leaves an empty tag in the list, as in "a,"; Forget refuses it.
func ParseTagList(s string) []string {
	tags := strings.Split(s, ",")
	for i, tag := range tags {
		tags[i] = strings.TrimSpace(tag)
	}
	return tags
}

// countedRule is one counted rule of a policy: its field name and count.
type countedRule struct {
	name string
	n    KeepCount
}

// counted lists the counted rules of p, KeepLast first, then the period
// rules in the order of periods, which forget_impl.go pairs them by.
func (p ForgetPolicy) counted() [6]countedRule {
	return [6]countedRule{
		{"KeepLast", p.KeepLast},
		{"KeepHourly", p.KeepHourly},
		{"KeepDaily", p.KeepDaily},
		{"KeepWeekly", p.KeepWeekly},
		{"KeepMonthly", p.KeepMonthly},
		{"KeepYearly", p.KeepYearly},
	}
}

// windowRule is one keep-within rule of a policy: its field name and window.
type windowRule struct {
	name string
	w    Within
}

// windows lists the keep-within rules of p, KeepWithin first, then the
// period rules in the order of periods, which forget_impl.go pairs them by.
func (p ForgetPolicy) windows() [6]windowRule {
	return [6]windowRule{
		{"KeepWithin", p.KeepWithin},
		{"KeepWithinHourly", p.KeepWithinHourly},
		{"KeepWithinDaily", p.KeepWithinDaily},
		{"KeepWithinWeekly", p.KeepWithinWeekly},
		{"KeepWithinMonthly", p.KeepWithinMonthly},
		{"KeepWithinYearly", p.KeepWithinYearly},
	}
}

// validate refuses a negative count other than Unlimited, a keep-within
// window with a number below zero or above its bound, a KeepTags list without
// a tag, which every snapshot would match, and one with the empty tag next
// to others, which none would: a stray comma must not turn a keep rule into
// one that silently keeps nothing.
func (p ForgetPolicy) validate() error {
	for _, c := range p.counted() {
		if c.n < Unlimited {
			return fmt.Errorf("%w: %s is %d, want 0 or more, or Unlimited", ErrInvalidPolicy, c.name, int(c.n))
		}
	}
	for _, r := range p.windows() {
		if err := r.w.validate(); err != nil {
			return fmt.Errorf("%w: %s.%w", ErrInvalidPolicy, r.name, err)
		}
	}
	for i, list := range p.KeepTags {
		if len(list) == 0 {
			return fmt.Errorf(`%w: KeepTags[%d] names no tag, want a tag, or "" for untagged`, ErrInvalidPolicy, i)
		}
		blank := func(tag string) bool { return strings.TrimSpace(tag) == "" }
		if len(list) > 1 && slices.ContainsFunc(list, blank) {
			return fmt.Errorf(
				`%w: KeepTags[%d] %q has an empty tag next to others, drop the stray comma; "" stands alone, for untagged`,
				ErrInvalidPolicy,
				i,
				strings.Join(list, ","),
			)
		}
	}
	return nil
}

// Empty reports whether p has no rule that keeps a snapshot. Forget takes
// such a policy only with snapshot IDs to remove, and refuses it with
// [ErrEmptyPolicy] otherwise. An unlimited count is a rule, and so is a tag
// list, although it may match nothing.
func (p ForgetPolicy) Empty() bool {
	for _, c := range p.counted() {
		if c.n != 0 {
			return false
		}
	}
	for _, r := range p.windows() {
		if r.w != (Within{}) {
			return false
		}
	}
	return len(p.KeepTags) == 0
}

// ForgetResult is returned by Repository.Forget.
type ForgetResult struct {
	Keep   []*Snapshot
	Remove []*Snapshot
	DryRun bool

	// KeepReasons names, per kept snapshot ID, every rule that selected it,
	// in the order the rules are applied and in restic's wording, such as
	// "has tags [a, b]", "within 30d", "last snapshot", "daily snapshot" or
	// "daily within 30d". A snapshot in Future has the one reason "in the
	// future". A snapshot that is not kept has no entry; when nothing is
	// kept the map may be nil or empty.
	KeepReasons map[SnapshotID][]string

	// Future lists the snapshots dated after the time of the run, newest
	// first. No rule of the policy saw them and all are kept, so each is in
	// Keep too. It is nil when there is none, and when Forget removes named
	// IDs without a policy, which removes such a snapshot like any other.
	Future []*Snapshot
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
// KeepYearly and KeepWithinHourly to KeepWithinYearly) count in: a snapshot
// belongs to the hour, day, week, month or year its time falls in on a clock
// in loc. The years, months and days of a keep-within window are stepped
// back on the same clock, so a day back across a summer time change is 23 or
// 25 hours. Without the option, and for a nil
// loc, Forget counts in [time.Local], so the same policy can keep different
// snapshots on machines in different zones; a program that must not depend on
// the machine passes a fixed zone such as [time.UTC]. KeepLast and the hours
// of a window do not depend on it.
func WithForgetLocation(loc *time.Location) ForgetOption {
	return func(c *forgetConfig) {
		if loc != nil {
			c.loc = loc
		}
	}
}
