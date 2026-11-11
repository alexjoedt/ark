package arkfs

import (
	"os"
	"syscall"
	"time"
)

func changeTime(stat *syscall.Stat_t) time.Time { return time.Unix(stat.Ctim.Unix()) }

// dataless is nil: Linux has no dataless files, so the walk reads no info for
// the cloud check.
var dataless func(os.FileInfo) bool
