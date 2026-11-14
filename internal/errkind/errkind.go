// Package errkind holds the few kinds of error that a caller maps to a
// response, such as an HTTP status. A package that can fail in one of
// these ways wraps the kind with its own message, so a caller handles the
// kind and a new error needs no change in the caller.
package errkind

import "errors"

// The kinds. Each is wrapped with detail, so compare with errors.Is.
var (
	// ErrInvalid means the input is not acceptable.
	ErrInvalid = errors.New("invalid input")
	// ErrNotFound means the thing asked for does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means the request clashes with the current state.
	ErrConflict = errors.New("conflict")
	// ErrTooBig means the input is over a size limit.
	ErrTooBig = errors.New("too big")
)

type kindError struct {
	kind error
	msg  string
}

func (e *kindError) Error() string { return e.msg }

// Unwrap returns the kind, so errors.Is finds it.
func (e *kindError) Unwrap() error { return e.kind }

// New returns an error with the message msg that is of the given kind.
// Declare a package's own sentinel with it, so the message is the
// package's and errors.Is matches both the sentinel and the kind.
func New(kind error, msg string) error {
	return &kindError{kind: kind, msg: msg}
}
