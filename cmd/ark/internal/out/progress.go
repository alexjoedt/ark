package out

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alexjoedt/ark"
)

// Progress renders the ProgressEvent stream of one operation. On a terminal
// it is a redrawn block: header with label and elapsed time, phase list,
// counts, current file. Without one it prints a line per phase; with
// --verbose it prints every entry; with --json or --quiet it is silent.
type Progress struct {
	p     *Printer
	label string
	start time.Time

	mu     sync.Mutex
	last   ark.ProgressEvent
	phase  ark.Phase
	seen   bool
	note   string
	ended  bool
	order  []ark.Phase
	marks  map[ark.Phase]phaseSeen
	seq    int
	cur    int // index in order of the furthest phase seen, -1 before
	counts ark.ProgressEvent
	final  bool // the backup scan sent its totals

	drawn int // lines of the block on screen, owned by spin
	stop  chan struct{}
	done  chan struct{}
}

// phaseSeen holds the sequence numbers of a phase's first and last event.
type phaseSeen struct{ first, last int }

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// phases lists the phases of each operation in order, as the library's
// Phase doc names them.
var phases = map[ark.Operation][]ark.Phase{
	ark.OperationBackup:  {ark.PhaseScan, ark.PhaseStore, ark.PhaseFinalize},
	ark.OperationRestore: {ark.PhaseLoad, ark.PhaseFetch, ark.PhaseWrite, ark.PhaseFinalize},
	ark.OperationForget:  {ark.PhaseLoad, ark.PhaseFinalize},
	ark.OperationPrune:   {ark.PhaseLoad, ark.PhaseStore, ark.PhaseSweep, ark.PhaseFinalize},
	ark.OperationVerify:  {ark.PhaseLoad, ark.PhaseFetch, ark.PhaseFinalize},
	ark.OperationCheck:   {ark.PhaseLoad, ark.PhaseFetch, ark.PhaseFinalize},
}

// etaAfter is the elapsed time before an ETA shows; earlier rates swing.
const etaAfter = 3 * time.Second

// Progress starts rendering for label ("backup /srv/app"). Call Done or Fail
// when the operation returns.
func (p *Printer) Progress(label string) *Progress {
	pr := &Progress{p: p, label: label, start: time.Now(), cur: -1, marks: map[ark.Phase]phaseSeen{}}
	if p.Live() {
		pr.stop = make(chan struct{})
		pr.done = make(chan struct{})
		go pr.spin()
	}
	return pr
}

// Func returns the callback to hand to the library. It never blocks.
func (pr *Progress) Func() ark.ProgressFunc {
	return pr.handle
}

func (pr *Progress) handle(ev ark.ProgressEvent) {
	pr.mu.Lock()
	if pr.ended {
		pr.mu.Unlock()
		return
	}
	prev, prevPhase, seen := pr.last, pr.phase, pr.seen
	pr.last, pr.phase, pr.seen = ev, ev.Phase, true
	pr.track(ev)
	pr.mu.Unlock()

	m := pr.p.mode
	switch {
	case m.JSON || m.Quiet:
		return
	case m.Verbose:
		if ev.CurrentPath != "" {
			fmt.Fprintf(pr.p.stderr, "%s %s\n", ev.Phase, ev.CurrentPath)
		} else if !seen || ev.Phase != prevPhase {
			fmt.Fprintln(pr.p.stderr, Line(ev))
		}
	case pr.stop == nil:
		// no terminal: one line when a phase ends, with its final counts
		if seen && ev.Phase != prevPhase {
			fmt.Fprintln(pr.p.stderr, Line(prev))
		}
	}
}

// track updates the block state; pr.mu is held. The counts follow the
// furthest phase in the operation's order; an event of an earlier phase that
// still runs (the backup scan, the restore fetch) only marks it running.
func (pr *Progress) track(ev ark.ProgressEvent) {
	if pr.order == nil {
		pr.order = phases[ev.Operation]
	}
	pr.seq++
	s, ok := pr.marks[ev.Phase]
	if !ok {
		s.first = pr.seq
	}
	s.last = pr.seq
	pr.marks[ev.Phase] = s
	if ev.Phase == ark.PhaseScan && (ev.FilesTotal > 0 || ev.BytesTotal > 0) {
		pr.final = true
	}
	i := slices.Index(pr.order, ev.Phase)
	if i < pr.cur {
		return
	}
	if i > pr.cur {
		pr.counts = ark.ProgressEvent{Operation: ev.Operation, Phase: ev.Phase}
		pr.cur = i
	}
	if ev.Phase == ark.PhaseScan {
		// The scan's totals are its own count; a percent of them says nothing.
		ev.FilesTotal, ev.BytesTotal = 0, 0
	}
	pr.counts = merge(pr.counts, ev)
}

// merge takes each counter pair and the path from ev where ev sets them, so
// a phase whose events carry different counters (check: chunks of one
// snapshot, then snapshots checked) keeps showing all of them.
func merge(c, ev ark.ProgressEvent) ark.ProgressEvent {
	if ev.FilesDone != 0 || ev.FilesTotal != 0 {
		c.FilesDone, c.FilesTotal = ev.FilesDone, ev.FilesTotal
	}
	if ev.BytesDone != 0 || ev.BytesTotal != 0 {
		c.BytesDone, c.BytesTotal = ev.BytesDone, ev.BytesTotal
	}
	if ev.ChunksDone != 0 || ev.ChunksTotal != 0 {
		c.ChunksDone, c.ChunksTotal = ev.ChunksDone, ev.ChunksTotal
	}
	if ev.CurrentPath != "" {
		c.CurrentPath = ev.CurrentPath
	}
	return c
}

func (pr *Progress) spin() {
	defer close(pr.done)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-pr.stop:
			fmt.Fprint(pr.p.stderr, pr.erase())
			return
		case <-t.C:
			lines := pr.lines(frames[i%len(frames)], pr.p.termWidth(), time.Since(pr.start))
			fmt.Fprint(pr.p.stderr, pr.erase()+strings.Join(lines, "\n"))
			pr.drawn = len(lines)
		}
	}
}

// erase moves the cursor to the first line of the drawn block and clears to
// the end of the screen. The block's last line ends without a newline, so the
// cursor sits on it.
func (pr *Progress) erase() string {
	switch pr.drawn {
	case 0:
		return ""
	case 1:
		return "\r\033[J"
	}
	return fmt.Sprintf("\r\033[%dA\033[J", pr.drawn-1)
}

// lines renders the block for a terminal width columns wide. Every line keeps
// a 2-column margin, so none wraps and the cursor-up count stays right.
func (pr *Progress) lines(frame string, width int, elapsed time.Duration) []string {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	w := max(width-2, 20)
	right := Duration(elapsed.Round(time.Second))
	left := cut(frame+" "+pr.label, w-utf8.RuneCountInString(right)-2)
	pad := w - utf8.RuneCountInString(left) - utf8.RuneCountInString(right)
	lines := []string{left + strings.Repeat(" ", max(pad, 2)) + right}
	add := func(s string) {
		if s != "" {
			lines = append(lines, "  "+cut(s, w-2))
		}
	}
	add(pr.phaseLine())
	if pr.note != "" {
		add(pr.note)
	} else if pr.seen {
		add(countsLine(pr.counts, elapsed))
	}
	add(pr.counts.CurrentPath)
	return lines
}

// phaseLine marks each phase of the operation: ✓ done, ▸ running, · pending.
// An earlier phase runs as long as it had an event after the furthest phase
// started, so overlapping phases both show ▸; the backup scan ends with its
// totals. A phase the operation skipped shows as done.
func (pr *Progress) phaseLine() string {
	if pr.cur < 0 {
		return ""
	}
	curFirst := pr.marks[pr.order[pr.cur]].first
	parts := make([]string, len(pr.order))
	for i, ph := range pr.order {
		s, ok := pr.marks[ph]
		mark := "✓"
		switch {
		case i > pr.cur:
			mark = "·"
		case ph == ark.PhaseScan && pr.final:
		case i == pr.cur, ok && s.last > curFirst:
			mark = "▸"
		}
		parts[i] = mark + " " + string(ph)
	}
	return strings.Join(parts, "   ")
}

// countsLine renders "12 345 / 40 000 files   3.1 / 10.0 GiB   31%   ETA 2m40s".
// Percent and ETA follow bytes, else chunks, else files, the first with a
// total; check takes snapshots, as its chunk totals cover one snapshot. The
// ETA is elapsed*(total-done)/done, shown from etaAfter on.
func countsLine(ev ark.ProgressEvent, elapsed time.Duration) string {
	unit := "files"
	if ev.Operation == ark.OperationCheck || ev.Operation == ark.OperationForget {
		unit = "snapshots"
	}
	var parts []string
	switch {
	case ev.FilesTotal > 0:
		parts = append(parts, Count(ev.FilesDone)+" / "+Count(ev.FilesTotal)+" "+unit)
	case ev.FilesDone > 0:
		parts = append(parts, Count(ev.FilesDone)+" "+unit)
	}
	switch {
	case ev.BytesTotal > 0:
		parts = append(parts, BytesOf(ev.BytesDone, ev.BytesTotal))
	case ev.BytesDone > 0:
		parts = append(parts, Bytes(ev.BytesDone))
	}
	switch {
	case ev.ChunksTotal > 0:
		parts = append(parts, Count(ev.ChunksDone)+" / "+Count(ev.ChunksTotal)+" chunks")
	case ev.ChunksDone > 0:
		parts = append(parts, Count(ev.ChunksDone)+" chunks")
	}
	var done, total int64
	switch {
	case ev.Operation == ark.OperationCheck:
		done, total = int64(ev.FilesDone), int64(ev.FilesTotal)
	case ev.BytesTotal > 0:
		done, total = ev.BytesDone, ev.BytesTotal
	case ev.ChunksTotal > 0:
		done, total = int64(ev.ChunksDone), int64(ev.ChunksTotal)
	default:
		done, total = int64(ev.FilesDone), int64(ev.FilesTotal)
	}
	if total > 0 {
		done = min(done, total)
		parts = append(parts, fmt.Sprintf("%d%%", done*100/total))
		if elapsed >= etaAfter && done > 0 && done < total {
			eta := time.Duration(float64(elapsed) * float64(total-done) / float64(done))
			parts = append(parts, "ETA "+Duration(eta.Round(time.Second)))
		}
	}
	return strings.Join(parts, "   ")
}

// cut shortens s to n runes by replacing its middle with "…". The part from
// the last "/" on, the base name of a path, is kept while it fits.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 2 {
		return string(r[:max(n, 0)])
	}
	keep := n - 1
	tail := keep / 2
	if i := strings.LastIndex(s, "/"); i >= 0 {
		tail = max(tail, utf8.RuneCountInString(s[i:]))
	}
	tail = min(tail, keep)
	return string(r[:keep-tail]) + "…" + string(r[len(r)-tail:])
}

// Wait replaces the counts line with msg until Resume. Without the live
// block it prints msg as one line to stderr, unless --quiet is set.
func (pr *Progress) Wait(msg string) {
	if pr.stop == nil {
		pr.p.Notef("%s", msg)
		return
	}
	pr.mu.Lock()
	pr.note = msg
	pr.mu.Unlock()
}

// Resume restores the counts line after Wait.
func (pr *Progress) Resume() {
	pr.mu.Lock()
	pr.note = ""
	pr.mu.Unlock()
}

func (pr *Progress) stopSpinner() {
	pr.mu.Lock()
	pr.ended = true
	pr.mu.Unlock()
	if pr.stop == nil {
		return
	}
	close(pr.stop)
	<-pr.done
	pr.stop = nil
}

// Done stops rendering, drops later events, and prints the summary block
// to stderr, unless --json or --quiet is set. The header is the label and the
// time since Progress started; rows follow, aligned.
func (pr *Progress) Done(rows ...Row) {
	pr.stopSpinner()
	var b strings.Builder
	fmt.Fprintf(&b, "%s  done in %s\n", pr.label, Duration(time.Since(pr.start)))
	w := 0
	for _, r := range rows {
		w = max(w, len(r.Key))
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-*s%s\n", w+3, r.Key, r.Value)
	}
	pr.p.Infof("%s", strings.TrimSuffix(b.String(), "\n"))
}

// Row is one line of the summary block.
type Row struct{ Key, Value string }

// R builds a Row with a formatted value.
func R(key, format string, args ...any) Row {
	return Row{key, fmt.Sprintf(format, args...)}
}

// Fail stops rendering and erases the block. The error is printed by main.
// It is safe to call again, and after Done.
func (pr *Progress) Fail() {
	pr.stopSpinner()
}

// Line renders one event as "<phase> <counts>", phases as identifiers.
func Line(ev ark.ProgressEvent) string {
	var b strings.Builder
	b.WriteString(string(ev.Phase))
	unit := "files"
	if ev.Operation == ark.OperationCheck || ev.Operation == ark.OperationForget {
		unit = "snapshots"
	}
	switch {
	case ev.FilesTotal > 0:
		fmt.Fprintf(&b, " %d/%d %s", ev.FilesDone, ev.FilesTotal, unit)
	case ev.FilesDone > 0:
		fmt.Fprintf(&b, " %d %s", ev.FilesDone, unit)
	}
	switch {
	case ev.BytesTotal > 0:
		fmt.Fprintf(&b, " %s/%s", Bytes(ev.BytesDone), Bytes(ev.BytesTotal))
	case ev.BytesDone > 0:
		fmt.Fprintf(&b, " %s", Bytes(ev.BytesDone))
	}
	switch {
	case ev.ChunksTotal > 0:
		fmt.Fprintf(&b, " %d/%d chunks", ev.ChunksDone, ev.ChunksTotal)
	case ev.ChunksDone > 0:
		fmt.Fprintf(&b, " %d chunks", ev.ChunksDone)
	}
	return b.String()
}
