package ark_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

type labeledSource struct {
	stubSource
	labels map[string]string
}

func (s labeledSource) Meta() ark.SourceMeta {
	m := s.stubSource.Meta()
	m.Labels = s.labels
	return m
}

// An invalid label stops the backup before the backend sees a single Put,
// the lock included.
func TestBackupRejectsInvalidLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		ok     bool
	}{
		{"plain key", map[string]string{"app": "my-app"}, true},
		{"prefixed key", map[string]string{"example.com/app": "x"}, true},
		{"empty value", map[string]string{"app": ""}, true},
		{"63 char name", map[string]string{strings.Repeat("a", 63): "x"}, true},
		{"1 KiB value", map[string]string{"app": strings.Repeat("v", 1024)}, true},
		{"free value", map[string]string{"app": "tcp://host:2375 ü"}, true},
		{"leading dash", map[string]string{"-app": "x"}, false},
		{"trailing dot", map[string]string{"app.": "x"}, false},
		{"space", map[string]string{"my app": "x"}, false},
		{"upper-case prefix", map[string]string{"Example.com/app": "x"}, false},
		{"empty prefix", map[string]string{"/app": "x"}, false},
		{"empty name", map[string]string{"example.com/": "x"}, false},
		{"two slashes", map[string]string{"a/b/c": "x"}, false},
		{"empty key", map[string]string{"": "x"}, false},
		{"64 char name", map[string]string{strings.Repeat("a", 64): "x"}, false},
		{"254 char prefix", map[string]string{strings.Repeat("a", 254) + "/app": "x"}, false},
		{"63 char prefix part", map[string]string{strings.Repeat("a", 63) + ".com/app": "x"}, true},
		{"64 char prefix part", map[string]string{strings.Repeat("a", 64) + ".com/app": "x"}, false},
		{"value over 1 KiB", map[string]string{"app": strings.Repeat("v", 1025)}, false},
		{"newline in value", map[string]string{"app": "a\nb"}, false},
		{"DEL in value", map[string]string{"app": "a\x7fb"}, false},
		{"invalid UTF-8", map[string]string{"app": "a\xffb"}, false},
		{"reserved prefix", map[string]string{"ark/path": "/srv"}, false},
		{"key the source sets", map[string]string{"docker_host": "x"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			b := &putCountingBackend{Backend: arkmem.New(), puts: map[ark.HandleType]int{}}
			repo := openRepo(t, b)
			b.take()
			src := labeledSource{
				stubSource: stubSource{name: "docs", content: "one"},
				labels:     map[string]string{"docker_host": "unix:///run/docker.sock"},
			}
			snap, err := repo.Backup(ctx, src, ark.WithLabels(tt.labels))
			if tt.ok {
				if err != nil {
					t.Fatalf("Backup: %v", err)
				}
				for k, v := range tt.labels {
					if snap.Labels[k] != v {
						t.Errorf("label %q = %q, want %q", k, snap.Labels[k], v)
					}
				}
				if snap.Labels["docker_host"] != src.labels["docker_host"] {
					t.Errorf("the source's label is lost: %v", snap.Labels)
				}
				return
			}
			if !errors.Is(err, ark.ErrInvalidLabel) {
				t.Fatalf("Backup error = %v, want ErrInvalidLabel", err)
			}
			if puts := b.take(); len(puts) != 0 {
				t.Errorf("the rejected backup wrote %v", puts)
			}
		})
	}
}

// Labels survive the manifest round trip, and a filter on them finds the
// snapshot through Snapshots.
func TestBackupLabelsAreStoredAndFiltered(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	src := labeledSource{
		stubSource: stubSource{name: "docs", content: "one"},
		labels:     map[string]string{"src": "s"},
	}
	labeled, err := repo.Backup(ctx, src, ark.WithLabels(map[string]string{"app": "my-app"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Backup(ctx, stubSource{name: "docs", content: "two"}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		labels map[string]string
		want   int
	}{
		{map[string]string{"app": "my-app"}, 1},
		{map[string]string{"app": "my-app", "src": "s"}, 1},
		{map[string]string{"app": "other"}, 0},
		{map[string]string{"app": "my-app", "env": "prod"}, 0},
		{nil, 2},
	} {
		snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{Labels: tt.labels})
		if err != nil {
			t.Fatal(err)
		}
		if len(snaps) != tt.want {
			t.Errorf("filter %v found %d snapshots, want %d", tt.labels, len(snaps), tt.want)
			continue
		}
		if tt.want != 1 {
			continue
		}
		if got := snaps[0]; got.ID != labeled.ID || got.Labels["src"] != "s" || got.Labels["app"] != "my-app" {
			t.Errorf("filter %v found %+v, want %s with both labels", tt.labels, got, labeled.ID)
		}
	}
}

// Several WithLabels calls add up, and the labels they set can be checked
// before Backup, as the Docker engine does before it stops containers.
func TestWithLabelsAddUp(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	snap, err := repo.Backup(ctx, stubSource{name: "docs", content: "one"},
		ark.WithLabels(map[string]string{"app": "x", "env": "dev"}),
		ark.WithLabels(map[string]string{"env": "prod"}))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Labels["app"] != "x" || snap.Labels["env"] != "prod" {
		t.Errorf("labels = %v, want app=x env=prod", snap.Labels)
	}
	if err := ark.ValidateBackupOptions(ark.WithTags("t"), ark.WithLabels(map[string]string{"app": "x"})); err != nil {
		t.Errorf("ValidateBackupOptions: %v", err)
	}
	err = ark.ValidateBackupOptions(
		ark.WithLabels(map[string]string{"app": "x"}),
		ark.WithLabels(map[string]string{"-bad": "x"}),
	)
	if !errors.Is(err, ark.ErrInvalidLabel) {
		t.Errorf("ValidateBackupOptions error = %v, want ErrInvalidLabel", err)
	}
}
