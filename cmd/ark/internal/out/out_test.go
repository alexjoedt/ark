package out

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
		pr.Done(R("snapshot", "x"))
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
	for _, k := range []string{"id", "created_at", "source_name", "source_type", "host", "tags", "comment", "excludes", "parent_id", "stats"} {
		if _, ok := got[k]; !ok {
			t.Errorf("key %q missing in %s", k, so.String())
		}
	}
	for _, k := range []string{"tags", "excludes"} {
		if v, ok := got[k].([]any); !ok || v == nil {
			t.Errorf("%s must be an empty array, got %v", k, got[k])
		}
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
	pr.Done(BackupRows(&ark.Snapshot{ID: "3f9a1c2e5b7d", Stats: ark.SnapshotStats{
		FileCount: 40000, FilesUnchanged: 39120, Skipped: 3, TotalSize: 10 << 30,
		NewChunksRawSize: 312 << 20, StoredSize: 201 << 20,
	}})...)
	want := "scan 5 files 2.0 KiB\nstore 5/5 files 2.0 KiB/2.0 KiB\n" +
		"backup /srv  done in <t>\n" +
		"  snapshot   3f9a1c2e\n" +
		"  files      40 000  (39 120 unchanged, 3 skipped)\n" +
		"  size       10.0 GiB\n" +
		"  new        312.0 MiB, stored as 201.0 MiB\n"
	got := regexp.MustCompile(`done in \S+`).ReplaceAllString(se.String(), "done in <t>")
	if got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
}

func TestProgressDoneAlignsRows(t *testing.T) {
	p, _, se := newTest(Mode{}, "")
	pr := p.Progress("prune")
	pr.Done(R("packs deleted", "%d", 2), R("freed", "%s", "1.0 KiB"))
	want := "prune  done in <t>\n  packs deleted   2\n  freed           1.0 KiB\n"
	got := regexp.MustCompile(`done in \S+`).ReplaceAllString(se.String(), "done in <t>")
	if got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
	p, _, se = newTest(Mode{}, "")
	p.Progress("prune").Fail()
	if se.Len() != 0 {
		t.Fatalf("block after a failure: %q", se.String())
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

func TestProgressBlockOnTerminal(t *testing.T) {
	p, _, se := newTest(Mode{}, "")
	p.SetTTY(true)
	pr := p.Progress("restore")
	pr.Func()(ark.ProgressEvent{Operation: ark.OperationRestore, Phase: ark.PhaseWrite, FilesDone: 1, FilesTotal: 2})
	time.Sleep(250 * time.Millisecond)
	pr.Done(R("snapshot", "x"))
	s := se.String()
	if !strings.Contains(s, "\r\033[2A\033[J") || !strings.Contains(s, "  1 / 2 files   50%") {
		t.Fatalf("no redrawn block in %q", s)
	}
	if !strings.HasSuffix(s, "\r\033[2A\033[Jrestore  done in "+s[strings.LastIndex(s, "done in ")+8:]) {
		t.Fatalf("block not erased before the summary: %q", s)
	}
}

func TestProgressBlockLines(t *testing.T) {
	p, _, _ := newTest(Mode{}, "")
	p.SetTTY(true)
	pr := p.Progress("backup /srv/app")
	f := pr.Func()
	f(ark.ProgressEvent{Operation: ark.OperationBackup, Phase: ark.PhaseScan})
	if got := pr.lines("⠹", 40, 0); len(got) != 2 || got[1] != "  ▸ scan   · store   · finalize" {
		t.Fatalf("scan lines %q", got)
	}
	f(
		ark.ProgressEvent{
			Operation:   ark.OperationBackup,
			Phase:       ark.PhaseStore,
			CurrentPath: "a",
			FilesDone:   1,
			BytesDone:   10,
		},
	)
	f(ark.ProgressEvent{Operation: ark.OperationBackup, Phase: ark.PhaseScan, FilesDone: 900})
	f(ark.ProgressEvent{
		Operation:   ark.OperationBackup,
		Phase:       ark.PhaseStore,
		CurrentPath: "srv/app/data/2026/11/exports/large-file.bin",
		FilesDone:   12345,
		FilesTotal:  40000,
		BytesDone:   3328599655,
		BytesTotal:  10737418240,
	})
	want := []string{
		"⠹ backup /srv/app" + strings.Repeat(" ", 56) + "1m12s",
		"  ✓ scan   ▸ store   · finalize",
		"  12 345 / 40 000 files   3.1 / 10.0 GiB   31%   ETA 2m40s",
		"  srv/app/data/2026/11/exports/large-file.bin",
	}
	if got := pr.lines("⠹", 80, 72*time.Second); !slices.Equal(got, want) {
		t.Fatalf("store lines\n%q\nwant\n%q", got, want)
	}
	f(
		ark.ProgressEvent{
			Operation:  ark.OperationBackup,
			Phase:      ark.PhaseScan,
			FilesDone:  40000,
			FilesTotal: 40000,
			BytesTotal: 10737418240,
		},
	)
	got := pr.lines("⠹", 30, 2*time.Second)
	want = []string{
		"⠹ backup /srv/app         2s",
		"  ✓ scan   ▸ st…  · finalize",
		"  12 345 / …/ 10.0 GiB   31%",
		"  srv/app/da…/large-file.bin",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("narrow lines\n%q\nwant\n%q", got, want)
	}
	pr.Wait("waiting for lock held by h:1 (exclusive)")
	if got := pr.lines("⠹", 80, 0); got[2] != "  waiting for lock held by h:1 (exclusive)" {
		t.Fatalf("waiting lines %q", got)
	}
	pr.Resume()
	if got := pr.lines("⠹", 80, 0); !strings.HasPrefix(got[2], "  12 345 / 40 000 files") {
		t.Fatalf("resumed lines %q", got)
	}
	pr.Fail()
	pr = p.Progress("backup x")
	f = pr.Func()
	f(
		ark.ProgressEvent{
			Operation:  ark.OperationBackup,
			Phase:      ark.PhaseScan,
			FilesDone:  3,
			FilesTotal: 3,
			BytesTotal: 9,
			BytesDone:  9,
		},
	)
	if got := pr.lines("⠹", 80, 0)[1:]; !slices.Equal(
		got,
		[]string{"  ✓ scan   · store   · finalize", "  3 files   9 B"},
	) {
		t.Fatalf("finished scan lines %q", got)
	}
	pr.Fail()
}

func TestProgressPhasesOverlapAndSkip(t *testing.T) {
	p, _, _ := newTest(Mode{}, "")
	pr := p.Progress("restore x")
	f := pr.Func()
	for _, ph := range []ark.Phase{ark.PhaseLoad, ark.PhaseFetch, ark.PhaseWrite, ark.PhaseFetch} {
		f(ark.ProgressEvent{Operation: ark.OperationRestore, Phase: ph})
	}
	if got := pr.lines("⠹", 80, 0)[1]; got != "  ✓ load   ▸ fetch   ▸ write   · finalize" {
		t.Fatalf("restore phases %q", got)
	}
	pr = p.Progress("prune")
	f = pr.Func()
	f(ark.ProgressEvent{Operation: ark.OperationPrune, Phase: ark.PhaseLoad})
	f(ark.ProgressEvent{Operation: ark.OperationPrune, Phase: ark.PhaseSweep, ChunksDone: 1, ChunksTotal: 4})
	if got := pr.lines("⠹", 80, 4*time.Second)[1:]; !slices.Equal(got, []string{
		"  ✓ load   · store   ▸ sweep   · finalize",
		"  1 / 4 chunks   25%   ETA 12s",
	}) {
		t.Fatalf("prune lines %q", got)
	}
	pr = p.Progress("check")
	f = pr.Func()
	f(
		ark.ProgressEvent{
			Operation:   ark.OperationCheck,
			Phase:       ark.PhaseFetch,
			CurrentPath: "01A",
			ChunksDone:  3,
			ChunksTotal: 3,
		},
	)
	f(
		ark.ProgressEvent{
			Operation:   ark.OperationCheck,
			Phase:       ark.PhaseFetch,
			CurrentPath: "01A",
			FilesDone:   1,
			FilesTotal:  4,
		},
	)
	if got := pr.lines("⠹", 80, 4*time.Second)[2]; got != "  1 / 4 snapshots   3 / 3 chunks   25%   ETA 12s" {
		t.Fatalf("check counts %q", got)
	}
}

func TestProgressPhaseMarksFollowEvents(t *testing.T) {
	p, _, _ := newTest(Mode{}, "")
	pr := p.Progress("restore x")
	ev := func(ph ark.Phase, at time.Duration) {
		pr.track(ark.ProgressEvent{Operation: ark.OperationRestore, Phase: ph}, at)
	}
	ev(ark.PhaseLoad, 0)
	ev(ark.PhaseWrite, 0)
	for _, c := range []struct {
		at   time.Duration
		ph   ark.Phase
		now  time.Duration
		want string
	}{
		{0, "", 0, "  ✓ load   · fetch   ▸ write   · finalize"},
		{time.Second, ark.PhaseFetch, time.Second, "  ✓ load   ▸ fetch   ▸ write   · finalize"},
		{time.Second, ark.PhaseWrite, 4 * time.Second, "  ✓ load   ✓ fetch   ▸ write   · finalize"},
	} {
		if c.ph != "" {
			ev(c.ph, c.at)
		}
		if got := pr.lines("⠹", 80, c.now)[1]; got != c.want {
			t.Errorf("at %s: %q, want %q", c.now, got, c.want)
		}
	}
}

func TestProgressETAFollowsThePhase(t *testing.T) {
	p, _, _ := newTest(Mode{}, "")
	pr := p.Progress("prune")
	pr.track(ark.ProgressEvent{Operation: ark.OperationPrune, Phase: ark.PhaseLoad}, 2*time.Minute)
	pr.track(ark.ProgressEvent{
		Operation: ark.OperationPrune, Phase: ark.PhaseSweep, ChunksDone: 1, ChunksTotal: 10,
	}, 3*time.Minute)
	if got := pr.lines("⠹", 80, 3*time.Minute+10*time.Second)[2]; got != "  1 / 10 chunks   10%   ETA 1m30s" {
		t.Fatalf("counts %q", got)
	}
}

func TestProgressBlockEscapesNames(t *testing.T) {
	p, _, _ := newTest(Mode{}, "")
	pr := p.Progress("backup \033[2J")
	pr.Func()(ark.ProgressEvent{
		Operation: ark.OperationBackup, Phase: ark.PhaseStore, CurrentPath: "a\nb\r\033[1A\x9b",
	})
	got := pr.lines("⠹", 80, 0)
	if !strings.HasPrefix(got[0], "⠹ backup ?[2J ") || got[len(got)-1] != "  a?b??[1A\ufffd" {
		t.Fatalf("lines %q", got)
	}
}

func TestProgressEraseCountsRewrappedRows(t *testing.T) {
	pr := &Progress{drawn: []int{100, 30}}
	if got := pr.erase(120); got != "\r\033[1A\033[J" {
		t.Fatalf("same width: %q", got)
	}
	if got := pr.erase(60); got != "\r\033[2A\033[J" {
		t.Fatalf("narrowed: %q", got)
	}
}

func TestProgressWaitWhileStopping(t *testing.T) {
	p, _, _ := newTest(Mode{}, "")
	p.SetTTY(true)
	pr := p.Progress("prune")
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			pr.Wait("waiting for lock")
		}
	})
	pr.Fail()
	pr.Fail()
	wg.Wait()
}

func TestCut(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"srv/app/data/2026/11/exports/large-file.bin", 30, "srv/app/data/2…/large-file.bin"},
		{"a/verylongbasename.txt", 10, "…ename.txt"},
		{"äöüäöüäöüäöü", 5, "äö…öü"},
	}
	for _, c := range cases {
		got := cut(c.in, c.n)
		if got != c.want || utf8.RuneCountInString(got) > c.n {
			t.Errorf("cut(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestProgressNoControlSequencesOffTerminal(t *testing.T) {
	events := []ark.ProgressEvent{
		{Operation: ark.OperationBackup, Phase: ark.PhaseScan},
		{Operation: ark.OperationBackup, Phase: ark.PhaseStore, CurrentPath: "a", FilesDone: 1},
		{Operation: ark.OperationBackup, Phase: ark.PhaseStore, CurrentPath: "b", FilesDone: 2},
		{Operation: ark.OperationBackup, Phase: ark.PhaseFinalize, FilesDone: 2},
	}
	for mode, want := range map[Mode]string{
		{}:              "scan\nstore 2 files\n",
		{Verbose: true}: "scan\nstore a\nstore b\nfinalize 2 files\n",
	} {
		p, so, se := newTest(mode, "")
		if mode.Verbose {
			p.SetTTY(true)
		}
		pr := p.Progress("backup")
		for _, ev := range events {
			pr.Func()(ev)
		}
		time.Sleep(150 * time.Millisecond)
		pr.Fail()
		if se.String() != want || so.Len() != 0 {
			t.Errorf("mode %+v: stderr %q stdout %q, want stderr %q", mode, se.String(), so.String(), want)
		}
	}
}

func TestProgressSweepAndCheckCounters(t *testing.T) {
	p, _, se := newTest(Mode{}, "")
	pr := p.Progress("prune")
	f := pr.Func()
	f(ark.ProgressEvent{Operation: ark.OperationPrune, Phase: ark.PhaseSweep, ChunksTotal: 9})
	f(ark.ProgressEvent{Operation: ark.OperationPrune, Phase: ark.PhaseSweep, ChunksDone: 9, ChunksTotal: 9})
	f(ark.ProgressEvent{Operation: ark.OperationPrune, Phase: ark.PhaseFinalize})
	pr.Fail()
	pr = p.Progress("check")
	f = pr.Func()
	f(
		ark.ProgressEvent{
			Operation:   ark.OperationCheck,
			Phase:       ark.PhaseFetch,
			CurrentPath: "01A",
			ChunksDone:  3,
			ChunksTotal: 3,
		},
	)
	f(
		ark.ProgressEvent{
			Operation:   ark.OperationCheck,
			Phase:       ark.PhaseFetch,
			CurrentPath: "01A",
			FilesDone:   1,
			FilesTotal:  2,
		},
	)
	f(ark.ProgressEvent{Operation: ark.OperationCheck, Phase: ark.PhaseFinalize, FilesDone: 2, FilesTotal: 2})
	pr.Fail()
	want := "sweep 9/9 chunks\nfetch 1/2 snapshots\n"
	if se.String() != want {
		t.Fatalf("stderr %q, want %q", se.String(), want)
	}
}

func TestProgressWait(t *testing.T) {
	p, _, se := newTest(Mode{JSON: true}, "")
	p.Progress("prune").Wait("waiting for lock held by h:1 (exclusive)")
	if se.String() != "waiting for lock held by h:1 (exclusive)\n" {
		t.Fatalf("stderr %q", se.String())
	}
}

func TestProgressDropsEventsAfterEnd(t *testing.T) {
	p, _, se := newTest(Mode{}, "")
	pr := p.Progress("forget")
	pr.Fail()
	f := pr.Func()
	f(ark.ProgressEvent{Phase: ark.PhaseFetch})
	f(ark.ProgressEvent{Phase: ark.PhaseFinalize})
	if se.String() != "" {
		t.Fatalf("stderr %q", se.String())
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
