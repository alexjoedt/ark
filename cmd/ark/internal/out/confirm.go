package out

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
)

// ErrDeclined is returned by Confirm when the user answers no.
var ErrDeclined = errors.New("cancelled")

// Asks reports whether Confirm puts a question to the user: a terminal, no
// --yes and no --json.
func (p *Printer) Asks() bool { return !p.mode.Yes && !p.mode.JSON && p.tty }

// Confirm asks "<prompt> [y/N]" on a terminal and returns nil on yes. --yes
// skips the question. Without a terminal, or with --json, a missing --yes
// is a usage error: a script never hangs on a prompt.
func (p *Printer) Confirm(prompt string) error {
	if p.mode.Yes {
		return nil
	}
	if !p.Asks() {
		return exitcode.Usagef("%s: confirmation needed, pass --yes", prompt)
	}
	fmt.Fprintf(p.stderr, "%s [y/N]: ", prompt)
	answer, err := bufio.NewReader(p.stdin).ReadString('\n')
	if err != nil && answer == "" {
		return ErrDeclined
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return ErrDeclined
}
