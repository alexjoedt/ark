package arkprofile_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark/arkprofile"
)

func echoHooks() arkprofile.Hooks {
	return arkprofile.Hooks{
		RunBefore:    "echo before",
		RunAfter:     "echo after",
		RunAfterFail: "echo after-fail",
		RunFinally:   "echo finally",
	}
}

func TestHooksRunInOrder(t *testing.T) {
	var out bytes.Buffer
	env := arkprofile.HookEnv{Output: &out}
	err := echoHooks().Run(t.Context(), env, func(context.Context) error {
		out.WriteString("step\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "before\nstep\nafter\nfinally\n"; got != want {
		t.Errorf("order %q, want %q", got, want)
	}
}

func TestHooksAfterAStepFailure(t *testing.T) {
	var out bytes.Buffer
	stepErr := errors.New("disk on fire")
	h := echoHooks()
	h.RunAfterFail = "echo $ARK_ERROR"
	err := h.Run(t.Context(), arkprofile.HookEnv{Output: &out}, func(context.Context) error { return stepErr })
	if !errors.Is(err, stepErr) {
		t.Errorf("error %v does not hold the step's", err)
	}
	if got, want := out.String(), "before\ndisk on fire\nfinally\n"; got != want {
		t.Errorf("order %q, want %q", got, want)
	}
}

func TestHooksFailingBeforeSkipsTheStep(t *testing.T) {
	var out bytes.Buffer
	h := echoHooks()
	h.RunBefore = "echo before; exit 3"
	called := false
	err := h.Run(t.Context(), arkprofile.HookEnv{Output: &out}, func(context.Context) error {
		called = true
		return nil
	})
	var he *arkprofile.HookError
	if !errors.As(err, &he) || he.Name != "run-before" || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error %v", err)
	}
	if called {
		t.Error("step ran after a failing run-before")
	}
	if got, want := out.String(), "before\nafter-fail\nfinally\n"; got != want {
		t.Errorf("order %q, want %q", got, want)
	}
}

func TestHooksFailingAfterReportsOnlyTheHook(t *testing.T) {
	var out bytes.Buffer
	h := echoHooks()
	h.RunAfter = "exit 1"
	err := h.Run(t.Context(), arkprofile.HookEnv{Output: &out}, func(context.Context) error { return nil })
	var he *arkprofile.HookError
	if !errors.As(err, &he) || he.Name != "run-after" {
		t.Fatalf("error %v", err)
	}
	if got, want := out.String(), "before\nfinally\n"; got != want {
		t.Errorf("order %q, want %q", got, want)
	}
}

func TestHooksSeeTheEnvironmentAndDirectory(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()
	h := arkprofile.Hooks{
		RunBefore:    `echo "$ARK_PROFILE $ARK_COMMAND [$ARK_ERROR]"; pwd -P`,
		RunAfterFail: `echo "$ARK_ERROR"`,
	}
	env := arkprofile.HookEnv{Dir: dir, Profile: "docs", Command: "backup", Output: &out}
	_ = h.Run(t.Context(), env, func(context.Context) error { return errors.New("boom") })
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "docs backup []\n"+wantDir+"\nboom\n"; got != want {
		t.Errorf("output %q, want %q", got, want)
	}
}

func TestHooksStopWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	h := arkprofile.Hooks{RunBefore: "sleep 30; sleep 30"}
	err := h.Run(ctx, arkprofile.HookEnv{}, func(context.Context) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s to stop", d)
	}
}
