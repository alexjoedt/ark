package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/alexjoedt/ark/cmd/ark/internal/ui/database"
)

func TestSessionStoreRoundTrip(t *testing.T) {
	db, err := database.New(filepath.Join(t.TempDir(), "ark.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := newSessionStore(db.DB(), time.Hour)

	if err := s.Commit("tok", []byte("data"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	b, ok, err := s.Find("tok")
	if err != nil || !ok || string(b) != "data" {
		t.Fatalf("find: %q %v %v", b, ok, err)
	}
	if err := s.Commit("old", []byte("x"), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Find("old"); ok {
		t.Fatal("expired session found")
	}
	if err := s.Delete("tok"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Find("tok"); ok {
		t.Fatal("deleted session found")
	}
}
