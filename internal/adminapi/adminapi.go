// Package adminapi is the admin JSON API at /api/v1. Everything it does to
// the server's data is done by the internal packages, which the command
// line uses too. This package only checks the caller's token or session,
// decodes the request, calls them, and encodes the result.
package adminapi

import (
	"cmp"
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

	"github.com/die-net/trmnl-byos-go/internal/adminauth"
	"github.com/die-net/trmnl-byos-go/internal/apitoken"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
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
	// FirmwareDir is where uploaded firmware is stored.
	FirmwareDir string
	// Zone is the server's time zone, which a device with none of its own is
	// in. Nil is UTC.
	Zone *time.Location
}

// Server handles admin API requests.
type Server struct {
	db  *db.Client
	cfg Config
	now func() time.Time
	// pushes rate limits pushes of data to instances.
	pushes pushLimiters
	// cop refuses a cross-origin request that rests on the session cookie.
	cop *http.CrossOriginProtection
}

// New returns a Server that keeps state in client.
func New(client *db.Client, cfg Config) *Server {
	cfg.Zone = cmp.Or(cfg.Zone, time.UTC)
	return &Server{db: client, cfg: cfg, now: time.Now, cop: http.NewCrossOriginProtection()}
}

// Register adds the /api/v1 routes to mux, and the /api/custom_plugins
// routes that scripts push data to, which keep TRMNL's path. Each one needs
// a bearer token or an admin session cookie, including a /api/v1 path that
// matches no route, so the API never says what exists to a caller without
// one.
func (s *Server) Register(mux *http.ServeMux) {
	routes := []struct {
		pattern string
		handler http.HandlerFunc
	}{
		{"GET /api/v1/devices", handle(http.StatusOK, s.listDevices)},
		{"GET /api/v1/devices/{id}", handle(http.StatusOK, s.getDevice)},
		{"PATCH /api/v1/devices/{id}", handle(http.StatusOK, s.patchDevice)},
		{"DELETE /api/v1/devices/{id}", handleNoContent(s.deleteDevice)},
		{"GET /api/v1/blocklist", handle(http.StatusOK, s.listBlocked)},
		{"PUT /api/v1/blocklist/{id}", handle(http.StatusOK, s.putBlocked)},
		{"DELETE /api/v1/blocklist/{id}", handleNoContent(s.deleteBlocked)},
		{"POST /api/v1/devices/{id}/calibrate", handle(http.StatusOK, s.calibrate)},
		{"GET /api/v1/join", handle(http.StatusOK, s.getJoin)},
		{"PUT /api/v1/join", handle(http.StatusOK, s.putJoin)},
		{"GET /api/v1/settings", handle(http.StatusOK, s.getSettings)},
		{"PATCH /api/v1/settings", handle(http.StatusOK, s.patchSettings)},
		{"GET /api/v1/firmware", handle(http.StatusOK, s.listFirmware)},
		{"POST /api/v1/firmware", handle(http.StatusCreated, s.addFirmware)},
		{"GET /api/v1/firmware/{id}/devices", handle(http.StatusOK, s.listFirmwareDevices)},
		{"POST /api/v1/firmware/{id}/rollout", handle(http.StatusOK, s.startRollout)},
		{"DELETE /api/v1/firmware/{id}/rollout", handleNoContent(s.cancelRollout)},
		{"GET /api/v1/recipes", handle(http.StatusOK, s.listRecipes)},
		{"GET /api/v1/recipes/{id}", handle(http.StatusOK, s.getRecipe)},
		{"POST /api/v1/recipes", handle(http.StatusCreated, s.installRecipe)},
		{"PUT /api/v1/recipes/{id}", handle(http.StatusOK, s.replaceRecipe)},
		{"GET /api/v1/instances", handle(http.StatusOK, s.listInstances)},
		{"GET /api/v1/instances/{id}", handle(http.StatusOK, s.getInstance)},
		{"POST /api/v1/instances", handle(http.StatusCreated, s.createInstance)},
		{"PATCH /api/v1/instances/{id}", handle(http.StatusOK, s.patchInstance)},
		{"DELETE /api/v1/instances/{id}", handleNoContent(s.deleteInstance)},
		{"GET /api/v1/playlists", handle(http.StatusOK, s.listPlaylists)},
		{"POST /api/v1/playlists", handle(http.StatusCreated, s.createPlaylist)},
		{"DELETE /api/v1/playlists/{id}", handleNoContent(s.deletePlaylist)},
		{"PUT /api/v1/playlists/{id}/devices/{device}", handle(http.StatusOK, s.linkPlaylistDevice)},
		{"PATCH /api/v1/playlists/{id}/devices/{device}", handle(http.StatusOK, s.movePlaylistDevice)},
		{"DELETE /api/v1/playlists/{id}/devices/{device}", handleNoContent(s.unlinkPlaylistDevice)},
		{"POST /api/v1/playlists/items", handle(http.StatusCreated, s.addPlaylistItem)},
		{"PATCH /api/v1/playlists/{id}", handle(http.StatusOK, s.patchPlaylist)},
		{"PATCH /api/v1/playlists/items/{id}", handle(http.StatusOK, s.patchPlaylistItem)},
		{"DELETE /api/v1/playlists/items/{id}", handleNoContent(s.deletePlaylistItem)},
		{"GET /api/custom_plugins/{id}", handle(http.StatusOK, s.getData)},
		{"POST /api/custom_plugins/{id}", handle(http.StatusOK, s.pushData)},
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

// errorBody is the body of every error response.
type errorBody struct {
	Error string `json:"error"`
	// Fields says what is wrong with each field of a request that has bad
	// custom field values, by keyname.
	Fields map[string]string `json:"fields,omitempty"`
	// Instances says what is wrong with each instance's values when a
	// recipe replace is refused, by instance ID, and then by keyname.
	Instances map[string]map[string]string `json:"instances,omitempty"`
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
	var fieldErrs recipe.FieldErrors
	var refused *recipe.ReplaceRefused
	switch {
	case errors.As(err, &status):
		maps.Copy(w.Header(), status.header)
		writeError(w, status.status, status.msg)
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body is over %d bytes", tooBig.Limit))
	case errors.Is(err, errkind.ErrTooBig):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.As(err, &fieldErrs):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Fields: fieldErrs})
	case errors.As(err, &refused):
		body := errorBody{Error: err.Error(), Instances: make(map[string]map[string]string, len(refused.Instances))}
		for id, errs := range refused.Instances {
			body.Instances[strconv.FormatInt(id, 10)] = errs
		}
		writeJSON(w, http.StatusBadRequest, body)
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

// auth runs next only for a request with a known bearer token, or with the
// cookie of a signed-in admin session. A bearer token wins: a request that
// sends one is judged by it alone, and is not checked for cross-origin use,
// since a browser does not add an Authorization header to a request another
// site makes. A request that rests on the cookie, which a browser does send
// then, passes the cross-origin check first. A token use is noted in the
// token's last_used_at, at most once per touchEvery.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		scheme, bearer, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		bearer = strings.TrimSpace(bearer)
		if !strings.EqualFold(scheme, "Bearer") || bearer == "" {
			s.authCookie(w, r, next)
			return
		}
		tok, err := s.db.Read.GetTokenByHash(ctx, apitoken.Hash(bearer))
		if errors.Is(err, sql.ErrNoRows) {
			unauthorized(w)
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

// authCookie is auth for a request with no bearer token.
func (s *Server) authCookie(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	signed, err := adminauth.SignedIn(r.Context(), s.db, r, s.now())
	if err != nil {
		fail(w, r, err)
		return
	}
	if !signed {
		unauthorized(w)
		return
	}
	if err := s.cop.Check(r); err != nil {
		writeError(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	next(w, r)
}

// unauthorized answers a request that has no valid credentials.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "a valid bearer token is required")
}
