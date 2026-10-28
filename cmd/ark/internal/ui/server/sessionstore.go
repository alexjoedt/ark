package server

import (
	"database/sql"
	"errors"
	"log/slog"
	"time"
)

// sessionStore keeps scs sessions in the sessions table. It is the
// alexedwards/scs sqlite3store (MIT, Alex Edwards) reduced to what ark
// uses; the upstream module drags the CGO sqlite driver into go.mod.
type sessionStore struct {
	db *sql.DB
}

// newSessionStore returns a store that removes expired rows every interval.
func newSessionStore(db *sql.DB, interval time.Duration) *sessionStore {
	s := &sessionStore{db: db}
	go func() {
		for range time.Tick(interval) {
			if _, err := db.Exec("DELETE FROM sessions WHERE expiry < julianday('now')"); err != nil {
				slog.Warn("session cleanup failed", "error", err)
			}
		}
	}()
	return s
}

func (s *sessionStore) Find(token string) ([]byte, bool, error) {
	var b []byte
	err := s.db.QueryRow("SELECT data FROM sessions WHERE token = $1 AND julianday('now') < expiry", token).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func (s *sessionStore) Commit(token string, b []byte, expiry time.Time) error {
	_, err := s.db.Exec("REPLACE INTO sessions (token, data, expiry) VALUES ($1, $2, julianday($3))",
		token, b, expiry.UTC().Format("2006-01-02T15:04:05.999"))
	return err
}

func (s *sessionStore) Delete(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token = $1", token)
	return err
}
