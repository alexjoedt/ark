package main

import (
	"time"

	"github.com/alexjoedt/ark"
)

// scenarios lists every history and policy the retention guide shows. The
// template reaches each one by its name.
func scenarios() []scenario {
	return []scenario{
		{name: "keep-last", history: running(), policy: ark.ForgetPolicy{KeepLast: 5}},
		{name: "keep-hourly", history: hours(), policy: ark.ForgetPolicy{KeepHourly: 4}},
		{name: "keep-daily", history: running(), policy: ark.ForgetPolicy{KeepDaily: 5}},
		{name: "keep-weekly", history: running(), policy: ark.ForgetPolicy{KeepWeekly: 4}},
		{name: "keep-monthly", history: months(), policy: ark.ForgetPolicy{KeepMonthly: 4}},
		{name: "keep-yearly", history: years(), policy: ark.ForgetPolicy{KeepYearly: 4}},
		{name: "keep-unlimited", history: running(), policy: ark.ForgetPolicy{KeepDaily: ark.Unlimited}},
		{name: "keep-within", history: running(), policy: ark.ForgetPolicy{KeepWithin: ark.Within{Days: 5}}},
		{
			name: "keep-within-hourly", history: hours(),
			policy: ark.ForgetPolicy{KeepWithinHourly: ark.Within{Hours: 3}},
		},
		{
			name: "keep-within-daily", history: running(),
			policy: ark.ForgetPolicy{KeepWithinDaily: ark.Within{Days: 7}},
		},
		{
			name: "keep-within-weekly", history: running(),
			policy: ark.ForgetPolicy{KeepWithinWeekly: ark.Within{Days: 14}},
		},
		{
			name: "keep-within-monthly", history: months(),
			policy: ark.ForgetPolicy{KeepWithinMonthly: ark.Within{Months: 3}},
		},
		{
			name: "keep-within-yearly", history: years(),
			policy: ark.ForgetPolicy{KeepWithinYearly: ark.Within{Years: 3}},
		},
		{name: "keep-tag", history: tagged(), policy: ark.ForgetPolicy{KeepTags: [][]string{{"release"}}}},
	}
}

// running is the history most scenarios share: three weeks of March 2015
// from Monday the 2nd, with days without a backup and days with several.
func running() []snap {
	return []snap{
		at("2015-03-02 09:00"),
		at("2015-03-02 18:00"),
		at("2015-03-03 09:00"),
		at("2015-03-03 18:00"),
		at("2015-03-04 18:00"),
		at("2015-03-06 12:00"),
		at("2015-03-06 18:00"),
		at("2015-03-06 23:00"),
		at("2015-03-07 18:00"),
		at("2015-03-09 18:00"),
		at("2015-03-10 18:00"),
		at("2015-03-12 09:00"),
		at("2015-03-12 18:00"),
		at("2015-03-13 18:00"),
		at("2015-03-16 18:00"),
		at("2015-03-18 08:00"),
		at("2015-03-18 13:00"),
		at("2015-03-18 18:00"),
		at("2015-03-19 18:00"),
		at("2015-03-20 18:00"),
		at("2015-03-22 18:00"),
	}
}

// hours is one working day of backups every half hour or so, with an
// hour without one.
func hours() []snap {
	return []snap{
		at("2015-03-02 08:10"),
		at("2015-03-02 08:40"),
		at("2015-03-02 09:05"),
		at("2015-03-02 09:35"),
		at("2015-03-02 09:55"),
		at("2015-03-02 10:20"),
		at("2015-03-02 12:15"),
		at("2015-03-02 12:45"),
		at("2015-03-02 13:10"),
		at("2015-03-02 13:30"),
		at("2015-03-02 13:50"),
	}
}

// months is two backups a month from January to August 2015, none in May.
func months() []snap {
	return []snap{
		at("2015-01-05 18:00"),
		at("2015-01-19 18:00"),
		at("2015-02-02 18:00"),
		at("2015-02-23 18:00"),
		at("2015-03-09 18:00"),
		at("2015-03-30 18:00"),
		at("2015-04-13 18:00"),
		at("2015-04-27 18:00"),
		at("2015-06-08 18:00"),
		at("2015-06-22 18:00"),
		at("2015-07-06 18:00"),
		at("2015-07-20 18:00"),
		at("2015-08-03 18:00"),
		at("2015-08-24 18:00"),
	}
}

// years is two backups a year from 2010 to 2015, none in 2013.
func years() []snap {
	return []snap{
		at("2010-03-01 18:00"),
		at("2010-09-06 18:00"),
		at("2011-02-14 18:00"),
		at("2011-11-07 18:00"),
		at("2012-05-21 18:00"),
		at("2012-12-03 18:00"),
		at("2014-04-07 18:00"),
		at("2014-10-13 18:00"),
		at("2015-02-02 18:00"),
		at("2015-03-02 18:00"),
	}
}

// tagged is a week of daily backups, two of them tagged release.
func tagged() []snap {
	return []snap{
		at("2015-03-02 18:00"),
		at("2015-03-03 18:00", "release"),
		at("2015-03-04 18:00"),
		at("2015-03-05 18:00"),
		at("2015-03-06 18:00", "release"),
		at("2015-03-07 18:00"),
		at("2015-03-08 18:00"),
	}
}

// at reads a snapshot time in UTC, such as "2015-03-02 18:00", and the
// snapshot's tags.
func at(when string, tags ...string) snap {
	t, err := time.Parse("2006-01-02 15:04", when)
	if err != nil {
		panic(err)
	}
	return snap{at: t, tags: tags}
}
