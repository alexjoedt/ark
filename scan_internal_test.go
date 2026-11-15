package ark

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

type unsupportedRewalk struct{ Source }

func (unsupportedRewalk) Rewalk(context.Context, func(FileInfo) error) error {
	return errors.ErrUnsupported
}

func TestScanOverUnsupportedRewalkIsSkipped(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var mu sync.Mutex
	events := 0
	opts := backupOptions{scan: true, progress: func(ProgressEvent) { events++ }}
	src := unsupportedRewalk{NewStreamSource("s", strings.NewReader(""))}

	startScan(context.Background(), src, opts, &mu, logger).stop()

	if !strings.Contains(buf.String(), "counting scan skipped") || strings.Contains(buf.String(), "dropped") {
		t.Errorf("log = %q, want the scan skipped, not dropped", buf.String())
	}
	if events != 0 {
		t.Errorf("%d scan events, want none", events)
	}
}
