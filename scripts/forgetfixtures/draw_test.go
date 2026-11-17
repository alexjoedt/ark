package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestFixtureInputsAreDrawn ties the checked-in fixture file to this
// generator: its histories and policies are the ones drawCase draws from the
// seed in its header, so a run with that seed and restic version writes the
// same questions again. A Go release that changes what math/rand/v2 derives
// from PCG fails it without anything wrong in ark; the replay test reads the
// stored cases and holds on.
func TestFixtureInputsAreDrawn(t *testing.T) {
	f, err := os.Open("../../testdata/forget/restic.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var head header
	if err := dec.Decode(&head); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(head.Restic, "restic "+resticVersion+" ") {
		t.Errorf("fixtures decided by %q, the generator pins restic %s", head.Restic, resticVersion)
	}
	for i := range head.Cases {
		var got fixtureCase
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		want, err := drawCase(head.Seed, i, "../../testdata/forget/zoneinfo")
		if err != nil {
			t.Fatal(err)
		}
		if len(want.Times) == 0 || len(want.Policy)+len(want.KeepTag) == 0 {
			t.Fatalf("case %d: drawn without a snapshot or without a rule", i)
		}
		if want.Times[0] >= unix(2026, 1, 1) {
			t.Fatalf("case %d: a snapshot at %d, restic has its own rule for the future", i, want.Times[0])
		}
		for j := 1; j < len(want.Times); j++ {
			if want.Times[j] >= want.Times[j-1] {
				t.Fatalf("case %d: times %d and %d are not newest first", i, j-1, j)
			}
		}
		// restic's answer is not drawn.
		got.Keep, got.Reasons, got.Refused = nil, nil, false
		gotLine, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		wantLine, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotLine) != string(wantLine) {
			t.Fatalf("case %d differs from the draw, the generator or math/rand/v2 changed:\n"+
				" file: %.300s\n draw: %.300s", i, gotLine, wantLine)
		}
	}
}
