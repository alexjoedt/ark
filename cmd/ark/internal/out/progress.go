package out

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/alexjoedt/ark"
)

// Progress renders the ProgressEvent stream of one operation. On a terminal
// it is a single redrawn line; without one it prints a line per phase; with
// --verbose it prints every entry; with --json or --quiet it is silent.
type Progress struct {
	p     *Printer
	label string

	mu    sync.Mutex
	last  ark.ProgressEvent
	phase ark.Phase
	seen  bool

	stop chan struct{}
	done chan struct{}
}

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Progress starts rendering for label ("backup /srv/app"). Call Donef or Fail
// when the operation returns.
func (p *Printer) Progress(label string) *Progress {
	pr := &Progress{p: p, label: label}
	if p.spinnerAllowed() {
		pr.stop = make(chan struct{})
		pr.done = make(chan struct{})
		go pr.spin()
	}
	return pr
}

func (p *Printer) spinnerAllowed() bool {
	return p.tty && !p.mode.JSON && !p.mode.Verbose && !p.mode.Quiet
}

// Func returns the callback to hand to the library. It never blocks.
func (pr *Progress) Func() ark.ProgressFunc {
	return pr.handle
}

func (pr *Progress) handle(ev ark.ProgressEvent) {
	pr.mu.Lock()
	prev, prevPhase, seen := pr.last, pr.phase, pr.seen
	pr.last, pr.phase, pr.seen = ev, ev.Phase, true
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

func (pr *Progress) spin() {
	defer close(pr.done)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-pr.stop:
			fmt.Fprint(pr.p.stderr, "\r\033[K")
			return
		case <-t.C:
			pr.mu.Lock()
			ev, seen := pr.last, pr.seen
			pr.mu.Unlock()
			line := pr.label
			if seen {
				line += ": " + Line(ev)
			}
			fmt.Fprintf(pr.p.stderr, "\r\033[K%s %s", frames[i%len(frames)], line)
		}
	}
}

func (pr *Progress) stopSpinner() {
	if pr.stop == nil {
		return
	}
	close(pr.stop)
	<-pr.done
	pr.stop = nil
}

// Donef stops rendering and prints the summary line, unless --json or
// --quiet is set.
func (pr *Progress) Donef(format string, args ...any) {
	pr.stopSpinner()
	pr.p.Infof(format, args...)
}

// Fail stops rendering and clears the line. The error is printed by main.
func (pr *Progress) Fail() {
	pr.stopSpinner()
}

// Line renders one event as "<phase> <counts>", phases as identifiers.
func Line(ev ark.ProgressEvent) string {
	var b strings.Builder
	b.WriteString(string(ev.Phase))
	switch {
	case ev.FilesTotal > 0:
		fmt.Fprintf(&b, " %d/%d files", ev.FilesDone, ev.FilesTotal)
	case ev.FilesDone > 0:
		fmt.Fprintf(&b, " %d files", ev.FilesDone)
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
