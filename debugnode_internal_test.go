package ark

import (
	"math"
	"testing"
)

func TestDebugEntryShowsTheStoredValueOfNoMtime(t *testing.T) {
	t.Parallel()
	if got := newDebugEntry(treeEntry{Name: "f", Type: FileTypeRegular}).MtimeNs; got != math.MinInt64 {
		t.Errorf("mtime_ns of an entry without an mtime = %d, want %d", got, int64(math.MinInt64))
	}
}
