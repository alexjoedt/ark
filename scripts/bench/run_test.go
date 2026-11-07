package main

import (
	"net/url"
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
		if r, v := verdict(c.ark, c.restic, true); r != c.ratio || v != c.v {
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
	a, r, err := sftpTools("sftp://ark@h:2222/repo?identity=/id", "ark", "restic")
	if err != nil || r == nil {
		t.Fatalf("sftpTools: %v, restic %v", err, r)
	}
	if got := r.repo(); !strings.HasPrefix(got, "sftp://ark@h:2222//repo/bench-restic-") {
		t.Errorf("restic repo = %s", got)
	}
	if got := a.repo(); !strings.HasPrefix(got, "sftp://ark@h:2222/repo/bench-ark-") ||
		!strings.Contains(got, "identity=") {
		t.Errorf("ark repo = %s", got)
	}
	a, r, err = sftpTools("sftp://[::1]?identity=/id", "ark", "restic")
	if err != nil || r == nil {
		t.Fatalf("sftpTools: %v, restic %v", err, r)
	}
	if got := r.repo(); !strings.HasPrefix(got, "sftp://[::1]//bench-restic-") {
		t.Errorf("restic repo without path = %s", got)
	}
	if got := a.repo(); !strings.HasPrefix(got, "sftp://[::1]/bench-ark-") {
		t.Errorf("ark repo without path = %s", got)
	}
}
