package main

import (
	"fmt"
	"strings"
	"sync/atomic"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

// activeProgress is the renderer of the operation now running. The library
// hooks are set once at Open, before the command knows its label, so they
// reach the renderer through this pointer. A CLI process runs one at a time.
var activeProgress atomic.Pointer[out.Progress]

// startProgress renders the operation that follows and routes the Open-time
// hooks to it.
func startProgress(p *out.Printer, label string) *out.Progress {
	pr := p.Progress(label)
	activeProgress.Store(pr)
	return pr
}

// hookOptions are the library hooks every command opens its repository with.
func hookOptions(cmd *cli.Command) []ark.Option {
	p := printer(cmd)
	// With no lock wait a rejection is final: the locked error says it all.
	waits := cmd.Root().Duration("lock-wait") > 0
	return []ark.Option{
		ark.WithProgress(func(ev ark.ProgressEvent) {
			if pr := activeProgress.Load(); pr != nil {
				pr.Func()(ev)
			}
		}),
		ark.WithLockEvent(func(ev ark.LockEvent) {
			pr := activeProgress.Load()
			switch ev.Kind {
			case ark.LockRejected:
				if !waits {
					return
				}
				if pr != nil {
					pr.Wait(lockWaitMessage(ev))
				} else {
					p.Notef("%s", lockWaitMessage(ev))
				}
			case ark.LockAcquired:
				if pr != nil {
					pr.Resume()
				}
			case ark.LockReleased, ark.LockLeftOver:
			}
		}),
	}
}

func lockWaitMessage(ev ark.LockEvent) string {
	if len(ev.Holders) == 0 {
		return "waiting for lock"
	}
	held := make([]string, len(ev.Holders))
	for i, h := range ev.Holders {
		class := "shared"
		if h.Exclusive {
			class = "exclusive"
		}
		held[i] = fmt.Sprintf("%s:%d (%s)", h.Hostname, h.PID, class)
	}
	return "waiting for lock held by " + strings.Join(held, ", ")
}
