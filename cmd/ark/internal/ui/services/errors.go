package services

import "strings"

// isUniqueConstraintError checks if a SQLite error is a unique constraint violation.
// This replaces brittle string matching against exact table/column names with a
// single check that works for any unique constraint across all tables.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
