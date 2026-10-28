// Package database provides SQLite database management for arkUI.
package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the sqlite driver
)

// Database wraps a SQLite connection with migrations and common operations.
// It provides a deep module interface - simple usage, complex implementation hidden.
type Database struct {
	db *sql.DB
}

// New creates a new Database instance and runs migrations.
// The dbPath should point to a SQLite database file (will be created if it doesn't exist).
// Parent directories are created automatically if they do not exist.
func New(dbPath string) (*Database, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0750); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	// Pragmas in the DSN apply to every pooled connection, a bare Exec
	// would only reach one of them.
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Run migrations
	if err := RunMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return &Database{db: db}, nil
}

// DB returns the underlying [sql.DB] instance.
// This is exposed for use with services that need direct database access.
func (d *Database) DB() *sql.DB {
	return d.db
}

// Close closes the database connection.
func (d *Database) Close() error {
	return d.db.Close()
}

// Ping checks if the database connection is alive.
func (d *Database) Ping() error {
	return d.db.Ping()
}
