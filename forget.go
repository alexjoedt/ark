package ark

import "time"

// ForgetPolicy defines the retention rules applied by Repository.Forget.
// The policy is applied per series: the snapshots that share source type,
// source name and host. A busy series never pushes another's snapshots out of
// the window, and two hosts backing up the same source each keep their own.
// Zero value keeps everything.
//
// The hours, days, weeks, months and years of KeepHourly to KeepYearly are
// those of the local time zone, not of UTC; see WithForgetLocation.
type ForgetPolicy struct {
	// KeepLast keeps the most recent n snapshots regardless of age.
	KeepLast int

	// KeepHourly keeps one snapshot per hour for the most recent n hours.
	KeepHourly int

	// KeepDaily keeps one snapshot per day for the most recent n days.
	KeepDaily int

	// KeepWeekly keeps one snapshot per ISO week for the most recent n weeks.
	KeepWeekly int

	// KeepMonthly keeps one snapshot per month for the most recent n months.
	KeepMonthly int

	// KeepYearly keeps one snapshot per year for the most recent n years.
	KeepYearly int

	// KeepWithin keeps every snapshot younger than this, measured from the
	// newest snapshot of the group rather than from the wall clock, so a
	// repository that stopped receiving backups does not lose everything.
	KeepWithin time.Duration
}

// keepsNothing reports whether no rule of p keeps a snapshot. A negative
// count or window keeps nothing, the same as a zero one.
func (p ForgetPolicy) keepsNothing() bool {
	return p.KeepLast <= 0 && p.KeepHourly <= 0 && p.KeepDaily <= 0 && p.KeepWeekly <= 0 &&
		p.KeepMonthly <= 0 && p.KeepYearly <= 0 && p.KeepWithin <= 0
}

// ForgetResult is returned by Repository.Forget.
type ForgetResult struct {
	Keep   []*Snapshot
	Remove []*Snapshot
	DryRun bool
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
