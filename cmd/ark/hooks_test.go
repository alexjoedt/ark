package main

import (
	"testing"

	ark "github.com/alexjoedt/ark"
)

func TestLockWaitMessage(t *testing.T) {
	ev := ark.LockEvent{Kind: ark.LockRejected, Holders: []ark.LockInfo{{Hostname: "h", PID: 7, Exclusive: true}}}
	if got, want := lockWaitMessage(ev), "waiting for lock held by h:7 (exclusive)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	ev.Holders[0].Exclusive = false
	if got, want := lockWaitMessage(ev), "waiting for lock held by h:7 (shared)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	ev.Holders = append(ev.Holders, ark.LockInfo{Hostname: "g", PID: 9})
	if got, want := lockWaitMessage(ev), "waiting for lock held by h:7 (shared), g:9 (shared)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
