package arkfs

import (
	"os"
	"syscall"
	"testing"
	"time"
)

type fakeInfo struct {
	os.FileInfo
	sys any
}

func (f fakeInfo) Sys() any         { return f.sys }
func (fakeInfo) Name() string       { return "f" }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() os.FileMode  { return 0o644 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return false }

func TestDatalessReadsStFlags(t *testing.T) {
	for _, tt := range []struct {
		name string
		sys  any
		want bool
	}{
		{"flag set", &syscall.Stat_t{Flags: sfDataless | 0x2}, true},
		{"other flags", &syscall.Stat_t{Flags: 0x2}, false},
		{"no flags", &syscall.Stat_t{}, false},
		{"foreign Sys", nil, false},
	} {
		if got := dataless(fakeInfo{sys: tt.sys}); got != tt.want {
			t.Errorf("%s: dataless = %v, want %v", tt.name, got, tt.want)
		}
	}
}
