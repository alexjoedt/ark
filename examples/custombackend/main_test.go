package main

import (
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arktest"
)

// TestConformance runs the suite the built-in backends pass.
func TestConformance(t *testing.T) {
	arktest.RunBackendTests(t, func(*testing.T) ark.Backend {
		return newKVBackend()
	})
}
