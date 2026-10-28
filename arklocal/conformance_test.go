package arklocal_test

import (
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/arktest"
)

func TestBackendConformance(t *testing.T) {
	arktest.RunBackendTests(t, func(t *testing.T) ark.Backend {
		b, err := arklocal.New(t.TempDir())
		if err != nil {
			t.Fatalf("arklocal.New: %v", err)
		}
		return b
	})
}
