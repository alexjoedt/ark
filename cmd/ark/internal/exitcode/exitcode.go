// Package exitcode maps errors to the exit codes and JSON error codes the ark
// CLI promises. Commands wrap errors, main decides the process exit.
package exitcode

import (
	"errors"
	"fmt"

	"github.com/alexjoedt/ark"
)

// Exit codes, part of the v1 contract.
const (
	OK            = 0
	Failure       = 1
	Usage         = 2
	Locked        = 3
	NotFound      = 4
	VerifyFailed  = 5
	WrongPassword = 6
)

// JSON error codes, part of the v1 contract.
const (
	KindError         = "error"
	KindUsage         = "usage"
	KindLocked        = "locked"
	KindNotFound      = "not_found"
	KindAmbiguousID   = "ambiguous_id"
	KindVerifyFailed  = "verify_failed"
	KindWrongPassword = "wrong_password"
)

// Error pins an exit code and a JSON code to an error. Unwrap keeps
// [errors.Is] working on the cause.
type Error struct {
	Code int
	Kind string
	Err  error
	// Silent means the command already printed what the user needs, main
	// exits with Code and writes nothing. Used with --json when a result
	// object stands in for the error shape.
	Silent bool
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Usagef returns a usage error, exit 2.
func Usagef(format string, args ...any) error {
	return &Error{Code: Usage, Kind: KindUsage, Err: fmt.Errorf(format, args...)}
}

// Verify returns a verification failure, exit 5.
func Verify(err error) error {
	return &Error{Code: VerifyFailed, Kind: KindVerifyFailed, Err: err}
}

// Reported returns a silent verification failure: the command printed the
// result, the process still exits 5.
func Reported() error {
	return &Error{
		Code:   VerifyFailed,
		Kind:   KindVerifyFailed,
		Err:    errors.New("verification found problems"),
		Silent: true,
	}
}

// IsSilent reports whether err carries a Silent Error.
func IsSilent(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Silent
}

// Of returns the exit code and JSON code for err. A pinned Error wins, then
// the library sentinels, then exit 1.
func Of(err error) (int, string) {
	if err == nil {
		return OK, ""
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code, e.Kind
	}
	switch {
	case errors.Is(err, ark.ErrRepoLocked):
		return Locked, KindLocked
	case errors.Is(err, ark.ErrNotFound):
		return NotFound, KindNotFound
	case errors.Is(err, ark.ErrAmbiguousID):
		return NotFound, KindAmbiguousID
	case errors.Is(err, ark.ErrWrongPassword):
		return WrongPassword, KindWrongPassword
	case errors.Is(err, ark.ErrEmptyPolicy),
		errors.Is(err, ark.ErrPasswordRequired),
		errors.Is(err, ark.ErrNotPasswordProtected):
		return Usage, KindUsage
	}
	return Failure, KindError
}
