// Package upload extends the deadlines of a request that carries a large
// body, such as a firmware image or a recipe zip.
package upload

import (
	"errors"
	"log/slog"
	"net/http"
	"time"
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
