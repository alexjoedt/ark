package out

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
)

func newTest(mode Mode, stdin string) (*Printer, *bytes.Buffer, *bytes.Buffer) {
	var so, se bytes.Buffer
	return NewWriter(mode, &so, &se, strings.NewReader(stdin)), &so, &se
}

func TestStreams(t *testing.T) {
	p, so, se := newTest(Mode{}, "")
	p.Infof("info %d", 1)
	p.Warnf("warn")
	p.Linef("line")
	p.Table([]string{"A", "B"}, [][]string{{"1", "22"}, {"333", "4"}})
	if se.String() != "info 1\nwarning: warn\n" {
		t.Fatalf("stderr %q", se.String())
	}
	want := "line\nA    B\n1    22\n333  4\n"
	if so.String() != want {
		t.Fatalf("stdout %q, want %q", so.String(), want)
	}
}

func TestQuietAndJSONSuppressHumanOutput(t *testing.T) {
	for _, mode := range []Mode{{Quiet: true}, {JSON: true}} {
		p, so, se := newTest(mode, "")
		p.Infof("info")
		pr := p.Progress("backup")
		pr.Func()(ark.ProgressEvent{Phase: ark.PhaseScan})
		pr.Func()(ark.ProgressEvent{Phase: ark.PhaseStore, FilesDone: 3})
		pr.Donef("done")
		if so.Len() != 0 || se.Len() != 0 {
			t.Fatalf("%+v wrote stdout %q stderr %q", mode, so.String(), se.String())
		}
	}
	// --quiet keeps the result on stdout, --json replaces it
	p, so, _ := newTest(Mode{Quiet: true}, "")
	p.Linef("line")
	if so.String() != "line\n" {
		t.Fatalf("quiet stdout %q", so.String())
	}
	p, so, _ = newTest(Mode{JSON: true}, "")
	p.Linef("line")
	p.Table([]string{"A"}, [][]string{{"1"}})
	if so.Len() != 0 {
		t.Fatalf("json stdout %q", so.String())
	}
	// warnings survive --json, on stderr
	p, so, se := newTest(Mode{JSON: true}, "")
	p.Warnf("careful")
	if so.Len() != 0 || se.String() != "warning: careful\n" {
		t.Fatalf("stdout %q stderr %q", so.String(), se.String())
	}
}

func TestJSONShapes(t *testing.T) {
	p, so, _ := newTest(Mode{JSON: true}, "")
	snap := &ark.Snapshot{
		ID: "01A", SourceName: "/srv", SourceType: ark.SourceTypeFile, Host: "h", CreatedAt: time.Unix(0, 0).UTC(),
	}
	if err := p.JSON(NewSnapshot(snap)); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(so.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "created_at", "source_name", "source_type", "host", "tags", "comment", "parent_id", "stats"} {
		if _, ok := got[k]; !ok {
			t.Errorf("key %q missing in %s", k, so.String())
		}
	}
	if tags, ok := got["tags"].([]any); !ok || tags == nil {
		t.Errorf("tags must be an empty array, got %v", got["tags"])
	}
	if len(Snapshots(nil)) != 0 || Snapshots(nil) == nil {
		t.Error("Snapshots(nil) must be an empty slice")
	}

	so.Reset()
	_ = p.JSONLine(map[string]int{"a": 1})
	_ = p.JSONLine(map[string]int{"b": 2})
	if so.String() != "{\"a\":1}\n{\"b\":2}\n" {
		t.Fatalf("JSONLine %q", so.String())
	}
}

func TestProgressWithoutTerminal(t *testing.T) {
	p, _, se := newTest(Mode{}, "")
	pr := p.Progress("backup /srv")
	f := pr.Func()
	f(ark.ProgressEvent{Phase: ark.PhaseScan})
	f(ark.ProgressEvent{Phase: ark.PhaseScan, FilesDone: 5, BytesDone: 2048, CurrentPath: "a"})
	f(ark.ProgressEvent{
		Phase: ark.PhaseStore, FilesDone: 1, FilesTotal: 5, BytesDone: 10, BytesTotal: 2048, CurrentPath: "a",
	})
	f(ark.ProgressEvent{Phase: ark.PhaseStore, FilesDone: 5, FilesTotal: 5, BytesDone: 2048, BytesTotal: 2048})
	f(ark.ProgressEvent{Phase: ark.PhaseFinalize, FilesDone: 5, BytesDone: 2048})
	pr.Donef("snapshot 01A")
	want := "scan 5 files 2.0 KiB\nstore 5/5 files 2.0 KiB/2.0 KiB\nsnapshot 01A\n"
	if se.String() != want {
		t.Fatalf("stderr %q, want %q", se.String(), want)
	}
}

func TestProgressVerbosePrintsPaths(t *testing.T) {
	p, _, se := newTest(Mode{Verbose: true}, "")
	pr := p.Progress("backup")
	f := pr.Func()
	f(ark.ProgressEvent{Phase: ark.PhaseScan})
	f(ark.ProgressEvent{Phase: ark.PhaseStore, CurrentPath: "dir/file", FilesDone: 1})
	f(ark.ProgressEvent{Phase: ark.PhaseFetch, ChunksDone: 2, ChunksTotal: 9})
	pr.Fail()
	want := "scan\nstore dir/file\nfetch 2/9 chunks\n"
	if se.String() != want {
		t.Fatalf("stderr %q, want %q", se.String(), want)
	}
}

func TestProgressSpinnerOnTerminal(t *testing.T) {
	p, _, se := newTest(Mode{}, "")
	p.SetTTY(true)
	pr := p.Progress("restore")
	pr.Func()(ark.ProgressEvent{Phase: ark.PhaseWrite, FilesDone: 1, FilesTotal: 2})
	time.Sleep(250 * time.Millisecond)
	pr.Donef("restored")
	s := se.String()
	if !strings.Contains(s, "\r\033[K") || !strings.Contains(s, "restore: write 1/2 files") {
		t.Fatalf("no spinner frame in %q", s)
	}
	if !strings.HasSuffix(s, "\r\033[Krestored\n") {
		t.Fatalf("line not cleared before the summary: %q", s)
	}
}

func TestConfirm(t *testing.T) {
	p, _, _ := newTest(Mode{Yes: true}, "")
	if err := p.Confirm("delete"); err != nil {
		t.Fatalf("--yes: %v", err)
	}

	p, _, _ = newTest(Mode{}, "y\n")
	if err := p.Confirm("delete"); !errors.Is(err, errUsage(err)) {
		t.Fatalf("no terminal must be a usage error, got %v", err)
	}
	p, _, _ = newTest(Mode{JSON: true}, "y\n")
	p.SetTTY(true)
	if code, _ := exitcode.Of(p.Confirm("delete")); code != exitcode.Usage {
		t.Fatal("--json without --yes must be a usage error")
	}

	for in, want := range map[string]error{"y\n": nil, "YES\n": nil, "n\n": ErrDeclined, "\n": ErrDeclined, "": ErrDeclined} {
		tp, _, se := newTest(Mode{}, in)
		tp.SetTTY(true)
		err := tp.Confirm("delete 3 snapshots")
		if !errors.Is(err, want) {
			t.Errorf("input %q: got %v, want %v", in, err, want)
		}
		if se.String() != "delete 3 snapshots [y/N]: " {
			t.Errorf("prompt %q", se.String())
		}
	}
}

// errUsage returns err itself when it carries exit code 2, so [errors.Is] holds.
func errUsage(err error) error {
	if code, _ := exitcode.Of(err); code == exitcode.Usage {
		return err
	}
	return errors.New("not a usage error")
}

func TestFormat(t *testing.T) {
	cases := map[string]string{
		Bytes(0):                          "0 B",
		Bytes(1023):                       "1023 B",
		Bytes(1024):                       "1.0 KiB",
		Bytes(1288490188):                 "1.2 GiB",
		Duration(1500 * time.Microsecond): "2ms",
		Duration(41*time.Second + 260*time.Millisecond): "41.3s",
		Duration(90 * time.Second):                      "1m30s",
		Count(12345):                                    "12 345",
		Count(999):                                      "999",
		Count(1000000):                                  "1 000 000",
		Ratio(0):                                        "1.0x",
		Ratio(0.93):                                     "14.3x",
		Ratio(1):                                        "inf",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	ts := time.Date(2026, 9, 9, 10, 12, 3, 0, time.Local)
	if Time(ts) != "2026-09-09 10:12" || TimeLong(ts) != "2026-09-09 10:12:03" {
		t.Errorf("Time %q TimeLong %q", Time(ts), TimeLong(ts))
	}
}

func TestParseBytes(t *testing.T) {
	ok := map[string]int64{
		"4096": 4096, "256KiB": 256 << 10, "1MiB": 1 << 20, "4M": 4 << 20,
		"2kb": 2048, "1.5G": 3 << 29, "512 B": 512,
	}
	for in, want := range ok {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MiB", "0", "-1M", "1X", "1.2.3K", "1 M B"} {
		if _, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) accepted", in)
		}
	}
}

func TestParseDuration(t *testing.T) {
	ok := map[string]time.Duration{
		"30d":  720 * time.Hour,
		"72h":  72 * time.Hour,
		"1.5d": 36 * time.Hour,
		"90m":  90 * time.Minute,
	}
	for in, want := range ok {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "0", "-1d", "30", "3 days"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) accepted", in)
		}
	}
}
