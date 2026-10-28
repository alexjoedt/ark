package database

import (
	"path/filepath"
	"testing"
)

func TestNewMigratesAndSetsPragmas(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "sub", "ark.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var n int
	if err := db.DB().
		QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sessions'").
		Scan(&n); err != nil ||
		n != 1 {
		t.Fatalf("sessions table: %d, %v", n, err)
	}
	var fk int
	if err := db.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys pragma: %d, %v", fk, err)
	}
	var mode string
	if err := db.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode: %q, %v", mode, err)
	}
	// a second open on the same file finds the schema in place
	again, err := New(filepath.Join(t.TempDir(), "ark.db"))
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}
