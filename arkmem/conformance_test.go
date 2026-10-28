package arkmem_test

import (
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/arktest"
)

func TestBackendConformance(t *testing.T) {
	arktest.RunBackendTests(t, func(_ *testing.T) ark.Backend {
		return arkmem.New()
	})
}
