package arkfs

import (
	"os"
	"syscall"
	"time"
)

func changeTime(stat *syscall.Stat_t) time.Time { return time.Unix(stat.Ctim.Unix()) }

func dataless(os.FileInfo) bool { return false }
