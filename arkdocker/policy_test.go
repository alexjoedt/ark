package arkdocker

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/moby/moby/api/types/container"
)

// unreachableEngine talks to a daemon that is not there, so any test that
// passes with it proves the daemon was never asked.
func unreachableEngine(t *testing.T, opts ...Option) *Engine {
	t.Helper()
	e, err := New(nil, append([]Option{WithDockerHost("tcp://127.0.0.1:1")}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestWithoutStopStillLooksUpContainers(t *testing.T) {
	e := unreachableEngine(t, WithoutStop())

	err := e.withContainersStopped(context.Background(), "vol", func([]ContainerInfo) error {
		t.Error("fn ran although the containers could not be listed")
		return nil
	})
	if err == nil {
		t.Fatal("withContainersStopped succeeded against an unreachable daemon")
	}
}

func TestStopPolicyListsContainersFirst(t *testing.T) {
	e := unreachableEngine(t)

	err := e.withContainersStopped(context.Background(), "vol", func([]ContainerInfo) error {
		t.Error("fn ran although the containers could not be listed")
		return nil
	})
	if err == nil {
		t.Fatal("withContainersStopped succeeded against an unreachable daemon")
	}
}

func TestContainerLabels(t *testing.T) {
	ctrs := []ContainerInfo{
		{
			Name:           "web",
			Image:          "nginx",
			ImageID:        "sha256:bbb",
			State:          "running",
			ComposeProject: "shop",
			ComposeService: "web",
		},
		{Name: "db", Image: "ghcr.io/x/pg:16", ImageID: "sha256:aaa", State: "exited"},
	}
	want := map[string]string{
		"ark/container.0.name":            "db",
		"ark/container.0.image":           "ghcr.io/x/pg:16",
		"ark/container.0.image_id":        "sha256:aaa",
		"ark/container.0.state":           "exited",
		"ark/container.1.name":            "web",
		"ark/container.1.image":           "nginx:latest",
		"ark/container.1.image_id":        "sha256:bbb",
		"ark/container.1.state":           "running",
		"ark/container.1.compose_project": "shop",
		"ark/container.1.compose_service": "web",
	}
	if got := containerLabels(ctrs); !maps.Equal(got, want) {
		t.Errorf("labels\n got %v\nwant %v", got, want)
	}
	if got := containerLabels(nil); len(got) != 0 {
		t.Errorf("no containers gave labels %v", got)
	}
}

func TestImageReference(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":           "nginx:latest",
		"nginx:1.27":      "nginx:1.27",
		"0123456789ab":    "0123456789ab",
		"reg.io:5000/a/b": "reg.io:5000/a/b:latest",
		"nginx@sha256:" + strings.Repeat("a", 64): "nginx@sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("a", 64):       "sha256:" + strings.Repeat("a", 64),
	} {
		if got := imageReference(in); got != want {
			t.Errorf("imageReference(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStopTimeoutDefaults(t *testing.T) {
	if got := unreachableEngine(t).config.StopTimeout; got != 30*time.Second {
		t.Errorf("default StopTimeout is %v, want 30s", got)
	}
	if got := unreachableEngine(t, WithStopTimeout(0)).config.StopTimeout; got != 30*time.Second {
		t.Errorf("WithStopTimeout(0) gives %v, want the default", got)
	}
	if got := unreachableEngine(t, WithStopTimeout(5*time.Second)).config.StopTimeout; got != 5*time.Second {
		t.Errorf("WithStopTimeout(5s) gives %v", got)
	}
}

func TestRunningContainersKeepsOnlyRunning(t *testing.T) {
	ctrs := []ContainerInfo{
		{Name: "a", State: string(container.StateRunning)},
		{Name: "b", State: string(container.StateExited)},
		{Name: "c", State: string(container.StatePaused)},
	}
	got := runningContainers(ctrs)
	if len(got) != 1 || got[0].Name != "a" {
		t.Errorf("got %v, want only a", got)
	}
}

func TestVolumeSourceCannotBeWalkedTwice(t *testing.T) {
	if _, ok := NewVolumeSource(&Client{}, "v").(ark.RewalkSource); ok {
		t.Error("the volume source implements ark.RewalkSource; its tar stream cannot be walked twice")
	}
}

func TestVolumeSourceLabelsAreReserved(t *testing.T) {
	ctrs := []ContainerInfo{{
		Name: "db", Image: "pg:16", ImageID: "sha256:a", State: "exited",
		ComposeProject: "p", ComposeService: "s",
	}}
	for _, host := range []string{"", "tcp://h:2375"} {
		labels := newVolumeSource(&Client{host: host}, "v", ctrs).Meta().Labels
		_, hasHost := labels["ark/docker-host"]
		if hasHost != (host != "") || labels["ark/docker-host"] != host {
			t.Errorf("host %q: ark/docker-host = %q (set %v)", host, labels["ark/docker-host"], hasHost)
		}
		for k := range labels {
			rest, ok := strings.CutPrefix(k, ark.ReservedLabelPrefix)
			if !ok {
				t.Errorf("key %q lacks the ark/ prefix", k)
			}
			if err := ark.ValidateLabels(map[string]string{"example.com/" + rest: labels[k]}); err != nil {
				t.Errorf("key %q has no valid shape: %v", k, err)
			}
		}
	}
}

func TestVolumeSourceMetaIsACopy(t *testing.T) {
	src := newVolumeSource(&Client{host: "tcp://h:2375"}, "v", nil)
	src.Meta().Labels["x/y"] = "z"
	if _, ok := src.Meta().Labels["x/y"]; ok {
		t.Error("a label added to Meta().Labels reached the source")
	}
}
