// Package errkind holds the few kinds of error that a caller maps to a
// response, such as an HTTP status. A package that can fail in one of
// these ways wraps the kind with its own message, so a caller handles the
// kind and a new error needs no change in the caller.
package errkind

import (
	"context"
	"errors"
)

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
	kind  error
	msg   string
	cause error
}

func (e *kindError) Error() string { return e.msg }

// Unwrap returns the kind, and the cause if there is one, so errors.Is and
// errors.As find both.
func (e *kindError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

// New returns an error with the message msg that is of the given kind.
// Declare a package's own sentinel with it, so the message is the
// package's and errors.Is matches both the sentinel and the kind.
func New(kind error, msg string) error {
	return &kindError{kind: kind, msg: msg}
}

// Wrap returns an error of the given kind with the message msg, and cause
// in its chain. The message does not repeat the cause, so it can be shown
// as it is when the cause is a Go error text that means nothing to a
// reader. errors.Is and errors.As still find the cause.
func Wrap(kind error, msg string, cause error) error {
	return &kindError{kind: kind, msg: msg, cause: cause}
}

// Ended reports whether err is ctx's own end, a cancel or a deadline, which
// is not a failure of the server. A request whose client hung up, and a job
// stopped by a shutdown, fail this way, and a caller that sees it logs no
// error and answers with no 500. An error that is not ctx's own, such as a
// timeout from a dependency while ctx is still live, is not Ended.
func Ended(ctx context.Context, err error) bool {
	cerr := ctx.Err()
	return cerr != nil && errors.Is(err, cerr)
}
