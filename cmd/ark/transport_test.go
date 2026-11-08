package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/alexjoedt/ark/cmd/ark/internal/config"
)

func TestServeDefaultsToLoopback(t *testing.T) {
	t.Setenv(config.EnvHTTPAddr, "")
	if config.DefaultHTTPAddr != "127.0.0.1:8080" {
		t.Errorf("DefaultHTTPAddr = %q, want 127.0.0.1:8080", config.DefaultHTTPAddr)
	}
	if _, err := os.Stat(config.DaemonConfigPath()); err == nil {
		t.Skip("a daemon.json on this machine overrides the default")
	}
	cfg, err := config.LoadServe(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != config.DefaultHTTPAddr {
		t.Errorf("LoadServe HTTPAddr = %q, want the default", cfg.HTTPAddr)
	}
	if w := serveWarnings(cfg); len(w) != 0 {
		t.Errorf("default config warns: %v", w)
	}
}

func TestServeWarnsOnANonLoopbackAddress(t *testing.T) {
	for addr, warns := range map[string]bool{
		"127.0.0.1:8080": false,
		"[::1]:8080":     false,
		"localhost:8080": false,
		":8080":          true,
		"0.0.0.0:8080":   true,
		"[::]:8080":      true,
		"192.168.1.5:80": true,
		"example.com:80": true,
		"garbage":        true,
	} {
		cfg := &config.ServeConfig{HTTPAddr: addr, CSRFEnabled: true}
		w := serveWarnings(cfg)
		if warns != (len(w) == 1) || (warns && !strings.Contains(w[0], "TLS reverse proxy")) {
			t.Errorf("addr %q: warnings %q, want warning %v", addr, w, warns)
		}
	}
}

func TestServeWarnsWhenCSRFIsOff(t *testing.T) {
	cfg := &config.ServeConfig{HTTPAddr: config.DefaultHTTPAddr, CSRFEnabled: true}
	cmd := &cli.Command{
		Name:  "serve",
		Flags: serveCommand().Flags,
		Action: func(_ context.Context, cmd *cli.Command) error {
			applyServeFlags(cfg, cmd)
			return nil
		},
	}
	if err := cmd.Run(t.Context(), []string{"serve", "--no-csrf"}); err != nil {
		t.Fatal(err)
	}
	w := serveWarnings(cfg)
	if len(w) != 1 || !strings.Contains(w[0], "--no-csrf") {
		t.Errorf("warnings with --no-csrf = %q, want one naming the flag", w)
	}
}

// TestNoPasswordFlag pins the passphrase inputs: --password-file, the
// environment and the prompt. A --password flag would show in the process
// list, so no command, hidden or not, may take the passphrase as a value.
func TestNoPasswordFlag(t *testing.T) {
	allowed := map[string]bool{
		"password-file":            true,
		"new-password-file":        true,
		"insecure-no-password":     true,
		"new-insecure-no-password": true,
		// the web UI login, env var only per its usage text
		"admin-password": true,
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
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code, out := runCapture(t, args...)
	os.Stderr = old
	w.Close()
	logs, _ := io.ReadAll(r)
	return code, out, string(logs)
}
