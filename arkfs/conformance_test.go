package arkfs_test

import (
	"path/filepath"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arktest"
)

func TestSourceSinkConformance(t *testing.T) {
	arktest.RunSourceSinkTests(t, ark.SourceTypeFile,
		func(t *testing.T, location string) ark.Source {
			t.Helper()
			src, err := arkfs.NewSource(location)
			if err != nil {
				t.Fatalf("NewSource(%s): %v", location, err)
			}
			return src
		},
		func(t *testing.T) (ark.Sink, string) {
			t.Helper()
			dir := filepath.Join(t.TempDir(), "restore")
			sink, err := arkfs.NewSink(dir)
			if err != nil {
				t.Fatalf("NewSink(%s): %v", dir, err)
			}
			return sink, dir
		},
	)
}
