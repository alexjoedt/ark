package ark_test

import (
	"testing"

	ark "github.com/alexjoedt/ark"
)

func TestSnapshotFilterMatchesType(t *testing.T) {
	snap := &ark.Snapshot{SourceName: "db", SourceType: "docker-volume"}
	tests := []struct {
		name string
		typ  ark.SourceType
		want bool
	}{
		{"empty matches all", "", true},
		{"same type", "docker-volume", true},
		{"other type", ark.SourceTypeFile, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (ark.SnapshotFilter{Type: tt.typ}).Match(snap); got != tt.want {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
		})
	}
}
