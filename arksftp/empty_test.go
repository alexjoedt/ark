package arksftp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEmpty(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) string
		want  bool
	}{
		{"missing", func(t *testing.T, dir string) string { return filepath.Join(dir, "new") }, true},
		{"empty dir", func(t *testing.T, dir string) string { return dir }, true},
		{"foreign file", func(t *testing.T, dir string) string {
			if err := os.WriteFile(filepath.Join(dir, "config"), []byte("restic"), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir
		}, false},
		// List skips these, yet a repository holding them is not empty: init
		// must not write over what another tool keeps there.
		{"foreign entries only", func(t *testing.T, dir string) string {
			if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "datapacks", "@eaDir"), 0o700); err != nil {
				t.Fatal(err)
			}
			return dir
		}, false},
		{"subdir only", func(t *testing.T, dir string) string {
			if err := os.Mkdir(filepath.Join(dir, "keys"), 0o700); err != nil {
				t.Fatal(err)
			}
			return dir
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := pipeBackend(t)
			b.basePath = tc.setup(t, b.basePath)
			got, err := b.Empty(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("Empty = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
