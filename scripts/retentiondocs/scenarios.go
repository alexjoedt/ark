package main

import (
	"time"

	"github.com/alexjoedt/ark"
)

// scenarios lists every history and policy the retention guide shows. The
// template reaches each one by its name.
func scenarios() []scenario {
	return []scenario{
		{
			name:    "keep-daily",
			history: running(),
			policy:  ark.ForgetPolicy{KeepDaily: 3},
		},
	}
}

// running is the history most scenarios share: three weeks of March 2015
// from Monday the 2nd, with days without a backup and days with several.
func running() []snap {
	return []snap{
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

// at reads a snapshot time in UTC, such as "2015-03-02 18:00".
func at(when string) snap {
	t, err := time.Parse("2006-01-02 15:04", when)
	if err != nil {
		panic(err)
	}
	return snap{at: t}
}
