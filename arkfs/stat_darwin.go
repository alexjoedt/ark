package arkfs

import (
	"syscall"
	"time"
)

func changeTime(stat *syscall.Stat_t) time.Time { return time.Unix(stat.Ctimespec.Unix()) }
