package main

import (
	"context"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"
)

func TestMedianSpreadVerdict(t *testing.T) {
	s := time.Second
	runs := []times{{10 * s, 4 * s, 6 * s}, {12 * s, 4 * s, 6 * s}, {11 * s, 4 * s, 6 * s}}
	if m := median(runs); m != (times{11 * s, 4 * s, 6 * s}) {
		t.Errorf("median = %v", m)
	}
	if got := spread(runs); got < 0.18 || got > 0.19 {
		t.Errorf("spread = %v, want 2/11", got)
	}
	for _, c := range []struct {
		ark, restic time.Duration
		ratio, v    string
	}{
		{12 * s, 10 * s, "1.20", "PASS"},
		{13 * s, 10 * s, "1.30", "FAIL"},
		{5 * s, 0, "-", ""},
	} {
		if r, v := verdict(c.ark, c.restic, budget); r != c.ratio || v != c.v {
			t.Errorf("verdict(%v, %v) = %s %s, want %s %s", c.ark, c.restic, r, v, c.ratio, c.v)
		}
	}
}

func TestSFTPCommand(t *testing.T) {
	for uri, want := range map[string]string{
		"sftp://ark@127.0.0.1:2222/repo?identity=/k/id&known_hosts=/k/kh": "ssh -p 2222 -i /k/id -o IdentitiesOnly=yes " +
			"-o BatchMode=yes -o UserKnownHostsFile=/k/kh ark@127.0.0.1 -s sftp",
		"sftp://host/srv?identity=/id&insecure=1": "ssh -p 22 -i /id -o IdentitiesOnly=yes -o BatchMode=yes " +
			"-o StrictHostKeyChecking=no host -s sftp",
		"sftp://ark@host/srv": "",
		"sftp://[::1]:2222/r?identity=/My%20Keys/id": "ssh -p 2222 -i '/My Keys/id' -o IdentitiesOnly=yes " +
			"-o BatchMode=yes ::1 -s sftp",
	} {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		if got := sftpCommand(u); got != want {
			t.Errorf("sftpCommand(%s) = %q, want %q", uri, got, want)
		}
	}
	a, r, err := sftpTools("sftp://ark@h:2222/repo?identity=/id", "ark", "restic", "/srv/m")
	if err != nil || r == nil {
		t.Fatalf("sftpTools: %v, restic %v", err, r)
	}
	if got, disk := r.repo(); !strings.HasPrefix(got, "sftp://ark@h:2222//repo/bench-restic-") ||
		disk != "/srv/m/"+path.Base(got) {
		t.Errorf("restic repo = %s on %s", got, disk)
	}
	if got, disk := a.repo(); !strings.HasPrefix(got, "sftp://ark@h:2222/repo/bench-ark-") ||
		!strings.Contains(got, "identity=") || !strings.HasPrefix(disk, "/srv/m/bench-ark-") {
		t.Errorf("ark repo = %s on %s", got, disk)
	}
	a, r, err = sftpTools("sftp://[::1]?identity=/id", "ark", "restic", "")
	if err != nil || r == nil {
		t.Fatalf("sftpTools: %v, restic %v", err, r)
	}
	if got, disk := r.repo(); !strings.HasPrefix(got, "sftp://[::1]//bench-restic-") || disk != "" {
		t.Errorf("restic repo without path = %s on %q", got, disk)
	}
	if got, _ := a.repo(); !strings.HasPrefix(got, "sftp://[::1]/bench-ark-") {
		t.Errorf("ark repo without path = %s", got)
	}
}

func TestSFTPDirNeedsRunAndURI(t *testing.T) {
	t.Setenv("ARK_SFTP_TEST_URI", "")
	for _, sub := range []string{"hash", "run"} {
		err := run(context.Background(), []string{sub, "-dir", t.TempDir(), "-sftp-dir", t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "-sftp-dir needs") {
			t.Errorf("%s: err = %v", sub, err)
		}
	}
}

func TestSummaryOneTool(t *testing.T) {
	var b strings.Builder
	s := time.Second
	runs := [][]result{{{t: times{10 * s, 4 * s, 6 * s}}, {t: times{14 * s, 4 * s, 6 * s}}}}
	summary(&b, "docker volume", []tool{{name: "ark"}}, runs, refBudget)
	out := b.String()
	for _, bad := range []string{"restic", "budget", "ratio", "verdict"} {
		if strings.Contains(out, bad) {
			t.Errorf("one-tool summary has %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "the medians are rough") || !strings.Contains(out, "initial      14.0s") {
		t.Errorf("one-tool summary:\n%s", out)
	}
}

func TestSFTPPlan(t *testing.T) {
	if title, last, judge := sftpPlan("sftp", sftpBudget, true); title != "sftp" || last != sftpRounds ||
		judge != sftpBudget {
		t.Errorf("mirrored = %q %d %v", title, last, judge)
	}
	if title, last, judge := sftpPlan("sftp", sftpBudget, false); !strings.Contains(title, "left on the server") ||
		last != 1 || judge != 0 {
		t.Errorf("unmirrored = %q %d %v", title, last, judge)
	}
}
