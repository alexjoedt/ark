package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// TestNoPasswordFlag pins the passphrase inputs: --password-file,
// --password-command, the
// environment and the prompt. A --password flag would show in the process
// list, so no command, hidden or not, may take the passphrase as a value.
func TestNoPasswordFlag(t *testing.T) {
	allowed := map[string]bool{
		"password-file":            true,
		"password-command":         true,
		"new-password-file":        true,
		"insecure-no-password":     true,
		"new-insecure-no-password": true,
	}
	var walk func(cmd *cli.Command)
	walk = func(cmd *cli.Command) {
		for _, f := range cmd.Flags {
			for _, n := range f.Names() {
				if strings.Contains(n, "pass") && !allowed[n] {
					t.Errorf("command %q: flag --%s may carry a passphrase in argv", cmd.Name, n)
				}
			}
		}
		for _, sub := range cmd.Commands {
			walk(sub)
		}
	}
	walk(newApp())
}

// TestPassphraseNeverInTheLogs runs the commands that touch the passphrase at
// debug level and looks for it, and for the new one, in everything written.
func TestPassphraseNeverInTheLogs(t *testing.T) {
	const pw, next = "hunter2-old-passphrase", "hunter2-new-passphrase"
	repo := filepath.Join(t.TempDir(), "repo")
	t.Setenv("ARK_REPOSITORY", "")
	t.Setenv("ARK_INSECURE_NO_PASSWORD", "")
	t.Setenv("ARK_PASSWORD", pw)
	t.Setenv("ARK_NEW_PASSWORD", next)

	var all strings.Builder
	runAll := func(want int, args ...string) {
		t.Helper()
		code, out, logs := runCaptureAll(t, append([]string{"-v", "-r", repo}, args...)...)
		all.WriteString(out + logs)
		if code != want {
			t.Fatalf("ark %v: exit %d, want %d\n%s", args, code, want, logs)
		}
	}
	runAll(0, "init")
	runAll(0, "snapshots")
	runAll(0, "key", "passwd")
	t.Setenv("ARK_PASSWORD", pw) // no longer opens
	runAll(6, "snapshots")
	t.Setenv("ARK_PASSWORD", next)
	runAll(0, "snapshots")

	for _, secret := range []string{pw, next} {
		if strings.Contains(all.String(), secret) {
			t.Errorf("output contains the passphrase %q", secret)
		}
	}
}

// runCaptureAll is runCapture that also returns what the command wrote to
// stderr, where the logger and the error message go.
func runCaptureAll(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var code int
	var out string
	logs := capture(t, &os.Stderr, func() { code, out = runCapture(t, args...) })
	return code, out, logs
}
