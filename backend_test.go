package ark

import "testing"

func TestHandleTypeString(t *testing.T) {
	cases := map[HandleType]string{
		ConfigFile:      "config",
		BlobFile:        "chunks",
		TreeFile:        "trees",
		SnapshotFile:    "snapshots",
		LockFile:        "locks",
		KeyFile:         "keys",
		HandleType(0):   "unknown",
		HandleType(200): "unknown",
	}
	for typ, want := range cases {
		if got := typ.String(); got != want {
			t.Errorf("HandleType(%d).String() = %q, want %q", typ, got, want)
		}
	}
}
