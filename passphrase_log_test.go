package ark_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// TestPassphraseNeverInTheLogs runs Init, Open, ChangePassword and a failed
// Open with a debug logger, on the repository and as the default, and looks
// for either passphrase in everything logged.
func TestPassphraseNeverInTheLogs(t *testing.T) {
	const pw, next = "hunter2-old-passphrase", "hunter2-new-passphrase"
	ctx := t.Context()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithPassword(pw)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, ark.WithPassword(pw), ark.WithLogger(logger))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()
	if err := repo.ChangePassword(ctx, ark.WithPassword(next)); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	failed, err := ark.Open(ctx, b, ark.WithPassword(pw), ark.WithLogger(logger))
	if err == nil {
		failed.Close()
		t.Fatal("Open with the replaced passphrase succeeded")
	}
	if strings.Contains(err.Error(), pw) {
		t.Errorf("Open error contains the passphrase: %v", err)
	}

	for _, secret := range []string{pw, next} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain the passphrase %q:\n%s", secret, logs.String())
		}
	}
}
