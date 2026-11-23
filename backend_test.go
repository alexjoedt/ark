package ark

import (
	"errors"
	"testing"
)

func TestHandleTypeString(t *testing.T) {
	cases := map[HandleType]string{
		ConfigFile:      "config",
		BlobFile:        "chunks",
		TreeFile:        "trees",
		SnapshotFile:    "snapshots",
		LockFile:        "locks",
		KeyFile:         "keys",
		DataPackFile:    "datapacks",
		TreePackFile:    "treepacks",
		DataIndexFile:   "dataindex",
		TreeIndexFile:   "treeindex",
		LostPackFile:    "lostpacks",
		HandleType(0):   "unknown",
		HandleType(200): "unknown",
	}
	for typ, want := range cases {
		if got := typ.String(); got != want {
			t.Errorf("HandleType(%d).String() = %q, want %q", typ, got, want)
		}
	}
}

func TestCheckName(t *testing.T) {
	cases := map[string]bool{
		"":           false,
		".":          false,
		"..":         false,
		"..x":        false,
		".ark-tmp-x": false,
		".DS_Store":  false,
		"a/b":        false,
		`a\b`:        false,
		"/a":         false,
		"a/":         false,
		"config":     true,
		"a":          true,
		"a.b":        true,
		"a..":        true,
		"3f9a0c":     true,
	}
	for name, ok := range cases {
		err := CheckName(name)
		if (err == nil) != ok {
			t.Errorf("CheckName(%q) = %v, want ok %v", name, err, ok)
		}
		if err != nil && !errors.Is(err, ErrInvalidName) {
			t.Errorf("CheckName(%q) = %v, want ErrInvalidName", name, err)
		}
	}
}
