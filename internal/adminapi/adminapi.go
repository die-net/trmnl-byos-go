// Package adminapi is the admin JSON API at /api/v1. Everything it does to
// the server's data is done by the internal packages, which the command
// line uses too. This package only checks the caller's token, decodes the
// request, calls them, and encodes the result.
package adminapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/apitoken"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

const (
	// maxJSONBody bounds a JSON request body.
	maxJSONBody = 64 << 10
	// touchEvery is the least time between updates of a token's
	// last_used_at, so a busy script does not write on every request.
	touchEvery = time.Minute
	// maxSeconds bounds a duration given in seconds, which also keeps it
	// from overflowing a time.Duration.
	maxSeconds = 365 * 24 * 3600
)

// Config is how the admin API is set up.
type Config struct {
	// Key seals and opens secret custom fields. It may be nil, which refuses
	// instances with secrets and cannot list the secrets of existing ones.
	Key *secrets.Key
	// RecipesDir is where uploaded recipes are installed.
	RecipesDir string
}

// Server handles admin API requests.
type Server struct {
	db  *db.Client
	cfg Config
	now func() time.Time
}

// New returns a Server that keeps state in client.
func New(client *db.Client, cfg Config) *Server {
	return &Server{db: client, cfg: cfg, now: time.Now}
}

// Register adds the /api/v1 routes to mux. Each one needs a bearer token,
// including a /api/v1 path that matches no route, so the API never says what
// exists to a caller without one.
func (s *Server) Register(mux *http.ServeMux) {
	routes := []struct {
		pattern string
		handler http.HandlerFunc
	}{
		{"GET /api/v1/devices", handle(http.StatusOK, s.listDevices)},
		{"GET /api/v1/devices/{id}", handle(http.StatusOK, s.getDevice)},
		{"POST /api/v1/devices/{id}/calibrate", handle(http.StatusOK, s.calibrate)},
		{"GET /api/v1/join", handle(http.StatusOK, s.getJoin)},
		{"PUT /api/v1/join", handle(http.StatusOK, s.putJoin)},
		{"GET /api/v1/recipes", handle(http.StatusOK, s.listRecipes)},
		{"GET /api/v1/recipes/{id}", handle(http.StatusOK, s.getRecipe)},
		{"POST /api/v1/recipes", handle(http.StatusCreated, s.installRecipe)},
		{"GET /api/v1/instances", handle(http.StatusOK, s.listInstances)},
		{"GET /api/v1/instances/{id}", handle(http.StatusOK, s.getInstance)},
		{"POST /api/v1/instances", handle(http.StatusCreated, s.createInstance)},
		{"DELETE /api/v1/instances/{id}", handleNoContent(s.deleteInstance)},
		{"GET /api/v1/playlists", handle(http.StatusOK, s.listPlaylists)},
		{"POST /api/v1/playlists/items", handle(http.StatusCreated, s.addPlaylistItem)},
		{"DELETE /api/v1/playlists/items/{id}", handleNoContent(s.deletePlaylistItem)},
		{"GET /api/v1/renders", handle(http.StatusOK, s.listRenders)},
		{"GET /api/v1/renders/{id}", handle(http.StatusOK, s.getRender)},
		{"/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "no such route")
		}},
	}
	for _, r := range routes {
		mux.Handle(r.pattern, s.auth(r.handler))
	}
}

// uploadTimeout is how long an upload may take to arrive. The server's own
// read and write timeouts are short, for everything else, and a recipe
// zip over a slow link needs longer.
const uploadTimeout = 10 * time.Minute

// allowSlowUpload gives the request its own read and write deadlines in
// place of the server's. The write deadline runs from the same start, since
// the server's is set when the request's headers arrive and would pass
// before a slow upload ends.
func allowSlowUpload(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	now := time.Now()
	if err := rc.SetReadDeadline(now.Add(uploadTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Warn("upload: extend read deadline", "err", err)
	}
	// A minute more, to answer.
	if err := rc.SetWriteDeadline(now.Add(uploadTimeout + time.Minute)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Warn("upload: extend write deadline", "err", err)
	}
}

// errorBody is the body of every error response.
type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "err", err)
	}
}

// handle adapts a handler that returns its response, or an error, into an
// http.HandlerFunc. A response is sent as JSON with the given status, and an
// error goes through fail. A handler that reads a body takes w for decode.
func handle[T any](status int, f func(w http.ResponseWriter, r *http.Request) (T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := f(w, r)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, status, v)
	}
}

// handleNoContent is handle for a handler with no response body, which
// answers 204.
func handleNoContent(f func(r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := f(r); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// statusError is an error a handler wants answered with a status other than
// those fail derives from an errkind kind, with its message, which is safe
// to show.
type statusError struct {
	status int
	msg    string
	// header is set on the response, such as Retry-After.
	header http.Header
}

func (e *statusError) Error() string { return e.msg }

// fail answers a request that failed with err. An error of an errkind kind
// has a safe message and a 4xx status: the mapping is here, so an error that
// wraps a kind needs no change. Any other error is logged, and the caller
// gets a generic message.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var tooBig *http.MaxBytesError
	var status *statusError
	switch {
	case errors.As(err, &status):
		maps.Copy(w.Header(), status.header)
		writeError(w, status.status, status.msg)
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body is over %d bytes", tooBig.Limit))
	case errors.Is(err, errkind.ErrTooBig):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, errkind.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errkind.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errkind.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		slog.Error("admin api", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// errBadBody marks a request body that is not the JSON the route takes.
var errBadBody = errkind.New(errkind.ErrInvalid, "bad request body")

// decode reads the JSON request body into v. It refuses a body over
// maxJSONBody, an unknown field, and anything after the JSON value. An
// empty body is an error unless emptyOK, which leaves v as it is.
func decode(w http.ResponseWriter, r *http.Request, v any, emptyOK bool) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if errors.Is(err, io.EOF) && emptyOK {
		return nil
	}
	if err == nil {
		// Only white space may follow the value, so a second Decode must
		// hit EOF. dec.More would miss a stray closer such as the second
		// brace of "{}}".
		var extra json.RawMessage
		switch err = dec.Decode(&extra); {
		case errors.Is(err, io.EOF):
			err = nil
		case err == nil:
			err = errors.New("unexpected data after the JSON value")
		}
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errBadBody, err)
	}
	return nil
}

// pathID parses the numeric {id} of a route. A malformed one can match
// nothing, so it is reported as unknown, with the error built by notFound.
func pathID(r *http.Request, notFound error) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", notFound, raw)
	}
	return id, nil
}

// duration converts a count of seconds to a duration, refusing a negative
// count or one over maxSeconds.
func duration(sec int64) (time.Duration, error) {
	if sec < 0 || sec > maxSeconds {
		return 0, fmt.Errorf("%w: %d seconds is not between 0 and %d", errBadBody, sec, maxSeconds)
	}
	return time.Duration(sec) * time.Second, nil
}

// auth runs next only for a request with a known bearer token. It notes
// the use in the token's last_used_at, at most once per touchEvery.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		scheme, bearer, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		bearer = strings.TrimSpace(bearer)
		var tok db.ApiToken
		var err error
		if strings.EqualFold(scheme, "Bearer") && bearer != "" {
			tok, err = s.db.Read.GetTokenByHash(ctx, apitoken.Hash(bearer))
		} else {
			err = sql.ErrNoRows
		}
		if errors.Is(err, sql.ErrNoRows) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "a valid bearer token is required")
			return
		}
		if err != nil {
			fail(w, r, fmt.Errorf("look up token: %w", err))
			return
		}
		if now := s.now(); now.Sub(time.Unix(tok.LastUsedAt, 0)) >= touchEvery {
			err := s.db.Write.TouchToken(ctx, db.TouchTokenParams{LastUsedAt: now.Unix(), ID: tok.ID})
			if err != nil {
				slog.Warn("update token last used", "token", tok.Name, "err", err)
			}
		}
		next(w, r)
	})
}
