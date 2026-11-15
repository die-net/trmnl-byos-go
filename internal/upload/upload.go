// Package upload extends the deadlines of a request that carries a large
// body, such as a firmware image or a recipe zip, and tells a client that
// dropped it from a server that failed.
package upload

import (
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// timeout is how long an upload may take to arrive. The server's own read
// and write timeouts are short, for everything else, and a firmware image
// or a recipe zip over a slow link needs longer.
const timeout = 10 * time.Minute

// AllowSlow gives the request its own read and write deadlines in place of
// the server's. The write deadline runs from the same start, since the
// server's is set when the request's headers arrive and would pass before a
// slow upload ends.
func AllowSlow(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	now := time.Now()
	if err := rc.SetReadDeadline(now.Add(timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Warn("upload: extend read deadline", "err", err)
	}
	// A minute more, to answer.
	if err := rc.SetWriteDeadline(now.Add(timeout + time.Minute)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Warn("upload: extend write deadline", "err", err)
	}
}

// ErrCutOff means the request body ended early or could not be read. It is
// invalid input, so a caller answers with a client error.
var ErrCutOff = errkind.New(errkind.ErrInvalid, "the upload was cut off or could not be read")

// Reader returns r with its read errors marked as the client's. Everything
// read through it comes from the request, so a read that fails means the
// client hung up, stalled past the deadline, or sent a broken body. A
// failure to write the upload to disk is not read through it, and stays a
// server error. A read error wraps ErrCutOff and keeps the cause, which
// the message leaves out. io.EOF, and the *http.MaxBytesError of a body
// over its limit, which callers answer with their own message, pass
// unchanged.
func Reader(r io.Reader) io.Reader {
	return reader{r}
}

type reader struct{ r io.Reader }

func (c reader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, err
	}
	return n, CutOff(err)
}

// CutOff marks err, which came from reading the request body, as ErrCutOff,
// for a caller that reads the body through a parser that returns its own
// errors, such as a multipart reader. nil and a *http.MaxBytesError are
// returned as they are.
func CutOff(err error) error {
	var tooBig *http.MaxBytesError
	if err == nil || errors.As(err, &tooBig) {
		return err
	}
	return errkind.Wrap(ErrCutOff, ErrCutOff.Error(), err)
}

// NextPart returns the next part of a multipart upload body. It returns
// nil and no error at the closing boundary, which is the clean end of the
// form. Any error is from a body that ended without one, is not a form, or
// could not be read, and is marked by CutOff. It is not io.EOF, which a
// cut-off error would also match.
func NextPart(mr *multipart.Reader) (*multipart.Part, error) {
	part, err := mr.NextPart()
	switch {
	case errors.Is(err, io.EOF) && errors.Unwrap(err) == nil:
		// An EOF the reader wraps ("NextPart: EOF") is a body that ended
		// without the closing boundary, which falls through to CutOff.
		return nil, nil
	case err != nil:
		return nil, CutOff(err)
	}
	return part, nil
}
