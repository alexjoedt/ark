package ark_test

import (
	"testing"
	"time"

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

// After includes the instant it names, Before excludes it.
func TestSnapshotFilterTimeBounds(t *testing.T) {
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		filter ark.SnapshotFilter
		at     time.Time
		want   bool
	}{
		{"at After", ark.SnapshotFilter{After: at}, at, true},
		{"a nanosecond before After", ark.SnapshotFilter{After: at}, at.Add(-time.Nanosecond), false},
		{"a nanosecond after After", ark.SnapshotFilter{After: at}, at.Add(time.Nanosecond), true},
		{"at Before", ark.SnapshotFilter{Before: at}, at, false},
		{"a nanosecond before Before", ark.SnapshotFilter{Before: at}, at.Add(-time.Nanosecond), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.filter.Match(&ark.Snapshot{CreatedAt: tt.at}); got != tt.want {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
		})
	}
}

// Every label of the filter must be on the snapshot with the same value.
func TestSnapshotFilterMatchesLabels(t *testing.T) {
	snap := &ark.Snapshot{Labels: map[string]string{"app": "my-app", "empty": ""}}
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"no labels", nil, true},
		{"same value", map[string]string{"app": "my-app"}, true},
		{"other value", map[string]string{"app": "other"}, false},
		{"one pair missing", map[string]string{"app": "my-app", "env": "prod"}, false},
		{"empty value present", map[string]string{"empty": ""}, true},
		{"empty value absent", map[string]string{"env": ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (ark.SnapshotFilter{Labels: tt.labels}).Match(snap); got != tt.want {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
		})
	}
}
