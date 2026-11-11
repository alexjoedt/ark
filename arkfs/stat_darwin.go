package arkfs

import (
	"os"
	"syscall"
	"time"
)

func changeTime(stat *syscall.Stat_t) time.Time { return time.Unix(stat.Ctimespec.Unix()) }

const sfDataless = 0x40000000 // SF_DATALESS in <sys/stat.h>

func dataless(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Flags&sfDataless != 0
}
