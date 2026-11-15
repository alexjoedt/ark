package arkprofile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// HookEnv is what the hooks of one command run see.
type HookEnv struct {
	// Dir is the working directory of every hook, normally the directory of
	// the profile file.
	Dir string
	// Profile and Command are passed as ARK_PROFILE and ARK_COMMAND, such as
	// "docs" and "backup".
	Profile, Command string
	// Output receives stdout and stderr of every hook; nil discards them.
	Output io.Writer
}

// HookError is the error of a failed hook. When a step's own error and a
// hook's error come back together, [errors.As] finds the hook by Name.
type HookError struct {
	// Name is the key of the hook, such as "run-before".
	Name string
	Err  error
}

func (e *HookError) Error() string { return e.Name + ": " + e.Err.Error() }
func (e *HookError) Unwrap() error { return e.Err }

// Run calls fn between the hooks. It runs run-before, then fn, then
// run-after when both succeeded, or run-after-fail when one of them failed,
// then run-finally in every case. A failing run-before skips fn. Each hook is
// a shell command run with sh -c in env.Dir, with ARK_PROFILE and
// ARK_COMMAND set and, for run-after-fail, ARK_ERROR holding the error text.
//
// The result joins the errors of fn and of the hooks, each hook as a
// [*HookError]. A failing run-after leaves fn's work done: the error is the
// hook's alone. Hooks have no timeout of their own. Cancelling ctx kills a
// running run-before or run-after and its children; run-after-fail and
// run-finally are the cleanup and run to their end even after a cancel, so a
// caller that must stop a hanging cleanup exits the process. A child a hook
// leaves in the background keeps the hook's output only when Output is nil or
// an [*os.File]; any other writer stops receiving a second after the hook exits.
func (h Hooks) Run(ctx context.Context, env HookEnv, fn func(context.Context) error) error {
	var stepErr error
	if err := h.run(ctx, env, "run-before", h.RunBefore, ""); err != nil {
		stepErr = err
	} else {
		stepErr = fn(ctx)
	}

	cleanup := context.WithoutCancel(ctx)
	var errs []error
	if stepErr != nil {
		errs = append(errs, stepErr)
		if err := h.run(cleanup, env, "run-after-fail", h.RunAfterFail, stepErr.Error()); err != nil {
			errs = append(errs, err)
		}
	} else if err := h.run(ctx, env, "run-after", h.RunAfter, ""); err != nil {
		errs = append(errs, err)
	}
	if err := h.run(cleanup, env, "run-finally", h.RunFinally, ""); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (h Hooks) run(ctx context.Context, env HookEnv, name, script, errText string) error {
	if script == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = env.Dir
	cmd.Env = append(os.Environ(), "ARK_PROFILE="+env.Profile, "ARK_COMMAND="+env.Command)
	if name == "run-after-fail" {
		cmd.Env = append(cmd.Env, "ARK_ERROR="+errText)
	}
	cmd.Stdout, cmd.Stderr = env.Output, env.Output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	killed := false
	cmd.Cancel = func() error {
		killed = true
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		return nil
	case killed:
		err = fmt.Errorf("%w: %w", ctx.Err(), err)
	}
	return &HookError{Name: name, Err: err}
}
