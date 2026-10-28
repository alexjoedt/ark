// Package out is the one place the CLI writes to. Machine output goes to
// stdout, everything a human reads while waiting goes to stderr, and the
// three modes --json, --verbose and --quiet decide what shows up at all.
package out

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"
)

// Mode holds the global output flags.
type Mode struct {
	JSON    bool
	Verbose bool
	Quiet   bool
	Yes     bool
}

// Printer renders for one command run.
type Printer struct {
	mode   Mode
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader
	tty    bool // stderr is a terminal: spinner and prompts are allowed
	ttyOut bool // stdout is a terminal: results carry a label
}

// New builds a Printer on the process streams.
func New(mode Mode) *Printer {
	p := NewWriter(mode, os.Stdout, os.Stderr, os.Stdin)
	p.tty = term.IsTerminal(int(os.Stderr.Fd())) && term.IsTerminal(int(os.Stdin.Fd()))
	p.ttyOut = term.IsTerminal(int(os.Stdout.Fd()))
	return p
}

// NewWriter builds a Printer on explicit streams, with no terminal. Tests
// use it; SetTTY flips the terminal behaviour on.
func NewWriter(mode Mode, stdout, stderr io.Writer, stdin io.Reader) *Printer {
	return &Printer{mode: mode, stdout: stdout, stderr: stderr, stdin: stdin}
}

// SetTTY overrides terminal detection.
func (p *Printer) SetTTY(tty bool) { p.tty = tty }

// Mode returns the flags the Printer was built with.
func (p *Printer) Mode() Mode { return p.mode }

// JSON writes v indented to stdout.
func (p *Printer) JSON(v any) error {
	enc := json.NewEncoder(p.stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

// JSONLine writes v as one line to stdout, for streamed output.
func (p *Printer) JSONLine(v any) error {
	if err := json.NewEncoder(p.stdout).Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

// Infof writes a line to stderr unless --quiet or --json is set.
func (p *Printer) Infof(format string, args ...any) {
	if p.mode.Quiet || p.mode.JSON {
		return
	}
	fmt.Fprintf(p.stderr, format+"\n", args...)
}

// Warnf writes a line to stderr unless --quiet is set. Warnings are for
// humans and do not corrupt --json output on stdout.
func (p *Printer) Warnf(format string, args ...any) {
	if p.mode.Quiet {
		return
	}
	fmt.Fprintf(p.stderr, "warning: "+format+"\n", args...)
}

// Linef writes a line of human output to stdout. Nothing is written with
// --json; the caller prints the JSON shape instead.
func (p *Printer) Linef(format string, args ...any) {
	if p.mode.JSON {
		return
	}
	fmt.Fprintf(p.stdout, format+"\n", args...)
}

// Result writes a command's result to stdout, "snapshot 01h..." on a
// terminal and the bare value otherwise, so $(ark backup .) captures the ID.
// Nothing is written with --json.
func (p *Printer) Result(label, value string) {
	if p.ttyOut {
		p.Linef("%s %s", label, value)
		return
	}
	p.Linef("%s", value)
}

// Table writes an aligned table to stdout, header first. An empty header
// renders a key-value block.
func (p *Printer) Table(header []string, rows [][]string) {
	if p.mode.JSON {
		return
	}
	w := tabwriter.NewWriter(p.stdout, 0, 0, 2, ' ', 0)
	if len(header) > 0 {
		fmt.Fprintln(w, strings.Join(header, "\t"))
	}
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	_ = w.Flush()
}
