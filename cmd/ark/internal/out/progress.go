package out

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
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

	live  bool // the block is drawn; set once in Printer.Progress
	once  sync.Once
	stop  chan struct{}
	done  chan struct{}
	drawn []int // columns of each line of the block on screen, owned by spin

	mu     sync.Mutex
	last   ark.ProgressEvent
	note   string
	ended  bool
	order  []ark.Phase
	marks  map[ark.Phase]phaseSeen
	seq    int           // events seen
	cur    int           // index in order of the furthest phase seen, -1 before
	curAt  time.Duration // when the furthest phase started, since start
	counts ark.ProgressEvent
	final  bool // the backup scan sent its totals
}

// phaseSeen holds the sequence numbers of a phase's first and last event and
// the time of the last one, since Progress started.
type phaseSeen struct {
	first, last int
	at          time.Duration
}

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

// etaAfter is the time in a phase before an ETA shows; earlier rates swing.
const etaAfter = 3 * time.Second

// runFor is how long an earlier phase shows as running after its last event.
const runFor = 2 * time.Second

// Progress starts rendering for label ("backup /srv/app"). Call Done or Fail
// when the operation returns.
func (p *Printer) Progress(label string) *Progress {
	pr := &Progress{p: p, label: label, start: time.Now(), cur: -1, marks: map[ark.Phase]phaseSeen{}}
	if p.Live() {
		pr.live = true
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
	prev, seen := pr.last, pr.seq > 0
	pr.last = ev
	pr.track(ev, time.Since(pr.start))
	pr.mu.Unlock()

	m := pr.p.mode
	switch {
	case m.JSON || m.Quiet:
		return
	case m.Verbose:
		if ev.CurrentPath != "" {
			fmt.Fprintf(pr.p.stderr, "%s %s\n", ev.Phase, printable(ev.CurrentPath))
		} else if !seen || ev.Phase != prev.Phase {
			fmt.Fprintln(pr.p.stderr, Line(ev))
		}
	case !pr.live:
		// no terminal: one line when a phase ends, with its final counts
		if seen && ev.Phase != prev.Phase {
			fmt.Fprintln(pr.p.stderr, Line(prev))
		}
	}
}

// track updates the block state; pr.mu is held. The counts follow the
// furthest phase in the operation's order; an event of an earlier phase that
// still runs (the backup scan, the restore fetch) only marks it running. at
// is the time of the event since Progress started.
func (pr *Progress) track(ev ark.ProgressEvent, at time.Duration) {
	if pr.order == nil {
		pr.order = phases[ev.Operation]
	}
	pr.seq++
	s, ok := pr.marks[ev.Phase]
	if !ok {
		s.first = pr.seq
	}
	s.last, s.at = pr.seq, at
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
		pr.cur, pr.curAt = i, at
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
			fmt.Fprint(pr.p.stderr, pr.erase(pr.p.termWidth()))
			return
		case <-t.C:
			w := pr.p.termWidth()
			lines := pr.lines(frames[i%len(frames)], w, time.Since(pr.start))
			// Autowrap is off while the block is written, so a line the rune
			// count underestimates (wide runes) is clipped, not wrapped.
			fmt.Fprint(pr.p.stderr, pr.erase(w)+"\033[?7l"+strings.Join(lines, "\n")+"\033[?7h")
			pr.drawn = pr.drawn[:0]
			for _, l := range lines {
				pr.drawn = append(pr.drawn, min(utf8.RuneCountInString(l), w))
			}
		}
	}
}

// erase moves the cursor to the first line of the drawn block and clears to
// the end of the screen. The block's last line ends without a newline, so the
// cursor sits on it. width is the terminal's width now: a line drawn wider
// takes more than one row once the terminal is narrowed.
// ponytail: assumes the terminal rewraps lines on resize (VTE, iTerm2, kitty,
// tmux); one that clips them (xterm) gets rows above the block cleared.
func (pr *Progress) erase(width int) string {
	rows := 0
	for _, c := range pr.drawn {
		rows += max(1, (c+width-1)/max(width, 1))
	}
	switch rows {
	case 0:
		return ""
	case 1:
		return "\r\033[J"
	}
	return fmt.Sprintf("\r\033[%dA\033[J", rows-1)
}

// lines renders the block for a terminal width columns wide. Every line keeps
// a 2-column margin, so none wraps and the cursor-up count stays right.
func (pr *Progress) lines(frame string, width int, elapsed time.Duration) []string {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	w := max(width-2, 20)
	right := Duration(elapsed.Round(time.Second))
	left := cut(frame+" "+printable(pr.label), w-utf8.RuneCountInString(right)-2)
	pad := w - utf8.RuneCountInString(left) - utf8.RuneCountInString(right)
	lines := []string{left + strings.Repeat(" ", max(pad, 2)) + right}
	add := func(s string) {
		if s != "" {
			lines = append(lines, "  "+cut(printable(s), w-2))
		}
	}
	add(pr.phaseLine(elapsed))
	if pr.note != "" {
		add(pr.note)
	} else if pr.seq > 0 {
		add(countsLine(pr.counts, max(elapsed-pr.curAt, 0)))
	}
	add(pr.counts.CurrentPath)
	return lines
}

// phaseLine marks each phase of the operation: ✓ done, ▸ running, · no event
// yet, so a phase the operation skips stays ·. An earlier phase runs while it
// had an event after the furthest phase started and within runFor of now, so
// overlapping phases both show ▸; the backup scan ends with its totals.
func (pr *Progress) phaseLine(now time.Duration) string {
	if pr.cur < 0 {
		return ""
	}
	curFirst := pr.marks[pr.order[pr.cur]].first
	parts := make([]string, len(pr.order))
	for i, ph := range pr.order {
		s, ok := pr.marks[ph]
		mark := "✓"
		switch {
		case i > pr.cur, !ok:
			mark = "·"
		case ph == ark.PhaseScan && pr.final:
		case i == pr.cur, s.last > curFirst && now-s.at < runFor:
			mark = "▸"
		}
		parts[i] = mark + " " + string(ph)
	}
	return strings.Join(parts, "   ")
}

// countParts renders the counters ev sets, done/total where a total is set.
// long is the live block's style ("12 345 / 40 000 files", "3.1 / 10.0 GiB"),
// else the line style ("12345/40000 files", "3.1 GiB/10.0 GiB").
func countParts(ev ark.ProgressEvent, long bool) []string {
	num, sep := strconv.Itoa, "/"
	bytesOf := func(done, total int64) string { return Bytes(done) + "/" + Bytes(total) }
	if long {
		num, sep, bytesOf = Count, " / ", BytesOf
	}
	unit := "files"
	if ev.Operation == ark.OperationCheck || ev.Operation == ark.OperationForget {
		unit = "snapshots"
	}
	var parts []string
	count := func(done, total int, name string) {
		switch {
		case total > 0:
			parts = append(parts, num(done)+sep+num(total)+" "+name)
		case done > 0:
			parts = append(parts, num(done)+" "+name)
		}
	}
	count(ev.FilesDone, ev.FilesTotal, unit)
	switch {
	case ev.BytesTotal > 0:
		parts = append(parts, bytesOf(ev.BytesDone, ev.BytesTotal))
	case ev.BytesDone > 0:
		parts = append(parts, Bytes(ev.BytesDone))
	}
	count(ev.ChunksDone, ev.ChunksTotal, "chunks")
	return parts
}

// countsLine renders "12 345 / 40 000 files   3.1 / 10.0 GiB   31%   ETA 2m40s".
// Percent and ETA follow bytes, else chunks, else files, the first with a
// total; check takes snapshots, as its chunk totals cover one snapshot. The
// ETA is elapsed*(total-done)/done, elapsed in the current phase, shown from
// etaAfter on.
func countsLine(ev ark.ProgressEvent, elapsed time.Duration) string {
	parts := countParts(ev, true)
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
// block, or once it ended, it prints msg as one line to stderr, unless
// --quiet is set.
func (pr *Progress) Wait(msg string) {
	pr.mu.Lock()
	shown := pr.live && !pr.ended
	if shown {
		pr.note = msg
	}
	pr.mu.Unlock()
	if shown {
		return
	}
	if pr.live {
		<-pr.done // the block is erased first
	}
	pr.p.Notef("%s", msg)
}

// Resume restores the counts line after Wait.
func (pr *Progress) Resume() {
	pr.mu.Lock()
	pr.note = ""
	pr.mu.Unlock()
}

// stopSpinner ends the block once; a concurrent call returns after the
// block is erased.
func (pr *Progress) stopSpinner() {
	pr.once.Do(func() {
		pr.mu.Lock()
		pr.ended = true
		pr.mu.Unlock()
		if pr.live {
			close(pr.stop)
			<-pr.done
		}
	})
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
	return strings.Join(append([]string{string(ev.Phase)}, countParts(ev, false)...), " ")
}

// printable replaces control characters, which would move the cursor or
// start an escape sequence, and invalid UTF-8 with a placeholder.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}
