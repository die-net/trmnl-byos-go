// Package adminui is the admin web UI: server-rendered pages from embedded
// templates, and the embedded CSS they use. It signs the admin in with the
// password and session cookie from adminauth. The pages call the packages
// the commands use, not the JSON API.
package adminui

import (
	"cmp"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/adminauth"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/render"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

//go:embed templates static
var assets embed.FS

const (
	// maxFormBody bounds a form post. The longest password is 1024 bytes.
	maxFormBody = 4 << 10

	// maxInstanceFormBody bounds a post of an instance's settings form. The
	// field values may total render.MaxEnvBytes, and form encoding can turn
	// each byte into six: a line break is sent as CR LF, and each of those
	// as "%XX". So the bound is six times that, plus room for the name,
	// palette, marks and the keynames of the fields.
	maxInstanceFormBody = 6*render.MaxEnvBytes + 64<<10

	// contentSecurityPolicy lets a page load only its own files, which
	// leaves out inline scripts and styles, and keeps it from being framed.
	contentSecurityPolicy = "default-src 'self'; frame-ancestors 'none'"
)

// Config is how the admin UI is set up.
type Config struct {
	// PublicURL is the base URL the server is reached at, if known. An
	// https one makes the session cookie Secure even when TLS ends at a
	// proxy, which the server cannot see.
	PublicURL string
	// Zone is the server's time zone, which times are shown and schedules
	// are read in. Nil is UTC.
	Zone *time.Location
	// RecipesDir is the directory recipes are installed under, which an
	// uploaded recipe is installed into.
	RecipesDir string
	// FirmwareDir is the directory firmware builds are stored in, which an
	// uploaded build is added to.
	FirmwareDir string
	// Secrets is the key that seals and opens the secret field values of
	// recipe instances. Nil means there is none, and then a form that sets a
	// secret value is refused by the domain.
	Secrets *secrets.Key
	// Rebuild, when not nil, is called after a change that builds read,
	// such as an instance's settings, so the scheduler rebuilds what it
	// makes due at once rather than at its next check.
	Rebuild func()
}

// Server handles admin UI requests.
type Server struct {
	db  *db.Client
	now func() time.Time
	// zone is Config.Zone, or UTC.
	zone *time.Location
	// recipesDir is Config.RecipesDir.
	recipesDir string
	// firmwareDir is Config.FirmwareDir.
	firmwareDir string
	// secrets is Config.Secrets.
	secrets *secrets.Key
	// rebuildFn is Config.Rebuild.
	rebuildFn func()
	// secureURL is whether PublicURL is https.
	secureURL bool
	// cop refuses a cross-origin request that is not safe.
	cop   *http.CrossOriginProtection
	pages map[string]*template.Template
}

// rebuild asks the scheduler to look for due renders now, through
// Config.Rebuild, after a change that builds read.
func (s *Server) rebuild() {
	if s.rebuildFn != nil {
		s.rebuildFn()
	}
}

// New returns a Server that keeps state in client.
func New(client *db.Client, cfg Config) *Server {
	s := &Server{
		db:          client,
		now:         time.Now,
		zone:        cmp.Or(cfg.Zone, time.UTC),
		recipesDir:  cfg.RecipesDir,
		firmwareDir: cfg.FirmwareDir,
		secrets:     cfg.Secrets,
		rebuildFn:   cfg.Rebuild,
		cop:         http.NewCrossOriginProtection(),
		pages:       map[string]*template.Template{},
	}
	if u, err := url.Parse(cfg.PublicURL); err == nil {
		s.secureURL = strings.EqualFold(u.Scheme, "https")
	}
	// Each page is the base layout with the page's own blocks, so pages
	// that define the same block do not clash.
	s.pages["login"] = template.Must(template.ParseFS(assets, "templates/base.html", "templates/login.html"))
	for _, name := range []string{"devices", "device", "device_delete", "unblock", "playlists", "playlist", "playlist_item", "playlist_item_delete", "playlist_delete", "recipes", "recipe", "instance", "instance_delete", "firmware", "firmware_build", "firmware_cancel", "settings", "notfound"} {
		s.pages[name] = template.Must(template.ParseFS(assets, "templates/base.html", "templates/shell.html", "templates/icons.html", "templates/screen.html", "templates/schedule.html", "templates/firmware_facts.html", "templates/instance_fields.html", "templates/standing.html", "templates/"+name+".html"))
	}
	return s
}

// Register adds the UI's routes to mux. The login page and the static files
// are public. Everything else needs a session.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("GET /login", page(http.HandlerFunc(s.loginForm)))
	mux.Handle("POST /login", page(s.cop.Handler(http.HandlerFunc(s.login))))
	mux.Handle("POST /logout", page(s.cop.Handler(http.HandlerFunc(s.logout))))
	mux.Handle("GET /{$}", page(s.requireSession(home)))
	mux.Handle("GET /devices", page(s.requireSession(s.devices)))
	mux.Handle("GET /devices/{id}", page(s.requireSession(s.deviceDetail)))
	mux.Handle("POST /devices/{id}", page(s.requireSession(s.updateDevice)))
	mux.Handle("POST /devices/{id}/playlists", page(s.requireSession(s.moveDevicePlaylist)))
	mux.Handle("POST /devices/{id}/block", page(s.requireSession(s.blockDevice)))
	mux.Handle("GET /devices/{id}/delete", page(s.requireSession(s.confirmDeleteDevice)))
	mux.Handle("POST /devices/{id}/delete", page(s.requireSession(s.deleteDevice)))
	mux.Handle("POST /blocklist", page(s.requireSession(s.blockMAC)))
	mux.Handle("GET /blocklist/{mac}", page(s.requireSession(s.confirmUnblock)))
	mux.Handle("POST /blocklist/{mac}", page(s.requireSession(s.unblockMAC)))
	mux.Handle("POST /join", page(s.requireSession(s.joinWindowForm)))
	mux.Handle("GET /recipes", page(s.requireSession(s.recipes)))
	mux.Handle("POST /recipes", page(s.requireSession(s.uploadRecipe)))
	mux.Handle("GET /recipes/{id}", page(s.requireSession(s.recipePage)))
	mux.Handle("POST /recipes/add-instance/{id}", page(s.requireSession(s.addInstance)))
	mux.Handle("GET /recipes/instances/{id}", page(s.requireSession(s.instancePage)))
	mux.Handle("POST /recipes/instances/{id}", page(s.requireSession(s.saveInstance)))
	mux.Handle("GET /recipes/instances/{id}/delete", page(s.requireSession(s.confirmRemoveInstance)))
	mux.Handle("POST /recipes/instances/{id}/delete", page(s.requireSession(s.removeInstance)))
	mux.Handle("GET /playlists", page(s.requireSession(s.playlists)))
	mux.Handle("POST /playlists", page(s.requireSession(s.createPlaylist)))
	mux.Handle("GET /playlists/{id}", page(s.requireSession(s.playlistPage)))
	mux.Handle("POST /playlists/{id}", page(s.requireSession(s.savePlaylist)))
	mux.Handle("GET /playlists/delete/{id}", page(s.requireSession(s.confirmDeletePlaylist)))
	mux.Handle("POST /playlists/delete/{id}", page(s.requireSession(s.deletePlaylist)))
	mux.Handle("POST /playlists/link/{id}", page(s.requireSession(s.linkDevice)))
	mux.Handle("POST /playlists/unlink/{id}", page(s.requireSession(s.unlinkDevice)))
	mux.Handle("POST /playlists/add-item/{id}", page(s.requireSession(s.addPlaylistItem)))
	mux.Handle("GET /playlists/items/{id}", page(s.requireSession(s.itemPage)))
	mux.Handle("POST /playlists/items/{id}", page(s.requireSession(s.saveItem)))
	mux.Handle("GET /playlists/items/{id}/delete", page(s.requireSession(s.confirmDeleteItem)))
	mux.Handle("POST /playlists/items/{id}/move", page(s.requireSession(s.itemAction(s.moveItem))))
	mux.Handle("POST /playlists/items/{id}/hidden", page(s.requireSession(s.itemAction(s.hideItem))))
	mux.Handle("POST /playlists/items/{id}/delete", page(s.requireSession(s.itemAction(s.deleteItem))))
	// Build pages are under /firmware/builds/ because the device download
	// route owns /firmware/{name}.
	mux.Handle("GET /firmware", page(s.requireSession(s.firmwareCatalog)))
	mux.Handle("POST /firmware", page(s.requireSession(s.uploadFirmware)))
	mux.Handle("GET /firmware/builds/{id}", page(s.requireSession(s.buildPage)))
	mux.Handle("POST /firmware/builds/{id}/rollout", page(s.requireSession(s.startRollout)))
	mux.Handle("GET /firmware/builds/{id}/cancel", page(s.requireSession(s.confirmCancelRollout)))
	mux.Handle("POST /firmware/builds/{id}/cancel", page(s.requireSession(s.cancelRollout)))
	mux.Handle("GET /settings", page(s.requireSession(s.settingsPage)))
	mux.Handle("POST /settings", page(s.requireSession(s.saveSetting)))
	mux.Handle("POST /settings/reset", page(s.requireSession(s.resetSetting)))
	mux.Handle("GET /static/", static())
}

// setSecurityHeaders sets what every response from the UI carries.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
}

// page wraps a handler of a page, which no cache may keep.
func page(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// static serves the embedded files under /static/. A directory is not
// listed.
func static() http.Handler {
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		// The files have no modification time to validate against, so
		// they are fetched again each time. They are small.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// requireSession runs next only for a request with a valid session cookie,
// and sends any other to the login page. A request that changes something
// also passes the cross-origin check, since the cookie is sent with a
// request that another site makes.
func (s *Server) requireSession(next http.HandlerFunc) http.Handler {
	return s.cop.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signed, err := adminauth.SignedIn(r.Context(), s.db, r, s.now())
		if err != nil {
			slog.Error("admin ui: check session", "err", err)
			s.render(w, r, http.StatusInternalServerError, "login", &pageData{Error: "Something went wrong. Try again."})
			return
		}
		if !signed {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}))
}

// pageData is what the templates are given.
type pageData struct {
	// Error is a message to show above the form or list, or "".
	Error string
	// Nav is the nav destination the page belongs to, such as "devices".
	Nav string
	// Heading is the page's title and its h1.
	Heading string
	// Devices is what the devices page lists, and Join the state of the
	// join window it shows.
	Devices []deviceView
	Join    joinView
	// Blocked is the MACs the devices page lists as blocked, and BlockForm
	// the form that blocks another.
	Blocked   []blockedView
	BlockForm blockForm
	// DeviceDelete is the device deletion its confirm page asks about, and
	// Unblock the unblock of a MAC its confirm page asks about.
	DeviceDelete *deviceDeleteView
	Unblock      *unblockView
	// Detail is the device the device page shows.
	Detail *deviceDetail
	// Playlists is what the playlists page lists, PlaylistEdit the playlist
	// its page shows, and ItemEdit the item its page shows.
	Playlists    []playlistView
	PlaylistEdit *playlistEdit
	ItemEdit     *itemEdit
	// ItemDelete is the item removal its confirm page asks about, and
	// PlaylistDelete the playlist deletion its confirm page asks about.
	ItemDelete     *itemDeleteView
	PlaylistDelete *playlistDeleteView
	// NewPlaylist is the new playlist form on the playlists page.
	NewPlaylist newPlaylistForm
	// Recipes is what the recipes page lists, and UploadLimit the size it
	// says an upload may be, such as "8 MB". The firmware page uses
	// UploadLimit too.
	Recipes     []recipeView
	UploadLimit string
	// UploadError is why an upload was refused, shown in the upload panel,
	// or "".
	UploadError string
	// RecipeEdit is the recipe its page shows.
	RecipeEdit *recipeEdit
	// InstanceEdit is the instance its page edits.
	InstanceEdit *instanceEdit
	// InstanceDelete is the instance removal the confirm page asks about.
	InstanceDelete *instanceDeleteView
	// Firmware is what the firmware page lists, FirmwareModels the models
	// its upload form offers, and UploadForm the values the form shows.
	Firmware       []firmwareView
	FirmwareModels []string
	UploadForm     *uploadForm
	// Build is the firmware build its page shows, and Cancel the rollout
	// cancel the confirm page asks about.
	Build  *buildView
	Cancel *cancelView
	// Settings is what the settings page lists.
	Settings []settingView
	// NotFound says what the not-found page could not find.
	NotFound notFoundView
}

// render sends the page called name with the status.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data *pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.pages[name].ExecuteTemplate(w, "base", data); err != nil {
		slog.Error("admin ui: render page", "page", name, "path", r.URL.Path, "err", err)
	}
}

// secure is whether the session cookie gets the Secure attribute.
func (s *Server) secure(r *http.Request) bool {
	return r.TLS != nil || s.secureURL
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if signed, _ := adminauth.SignedIn(r.Context(), s.db, r, s.now()); signed {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", &pageData{})
}

// parseForm reads a posted form of at most maxFormBody bytes. If it cannot,
// it calls fail with a status and a message to show, and returns false.
func parseForm(w http.ResponseWriter, r *http.Request, fail func(status int, msg string)) bool {
	return parseFormLimit(w, r, maxFormBody, fail)
}

// parseFormLimit is parseForm with a bound of limit bytes.
func parseFormLimit(w http.ResponseWriter, r *http.Request, limit int64, fail func(status int, msg string)) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	err := r.ParseForm()
	if err == nil {
		return true
	}
	status, msg := http.StatusBadRequest, "The form could not be read."
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		status, msg = http.StatusRequestEntityTooLarge, fmt.Sprintf("The form is over %d bytes.", tooBig.Limit)
	}
	fail(status, msg)
	return false
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r, func(status int, msg string) { s.render(w, r, status, "login", &pageData{Error: msg}) }) {
		return
	}
	token, err := adminauth.Login(r.Context(), s.db, r.PostForm.Get("password"), s.now())
	switch {
	case err == nil:
		http.SetCookie(w, adminauth.NewCookie(token, s.secure(r)))
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case errors.Is(err, adminauth.ErrNoPassword):
		s.render(w, r, http.StatusUnauthorized, "login", &pageData{Error: "No admin password is set. Run trmnl-byos password on the server."})
	case errors.Is(err, adminauth.ErrWrongPassword):
		s.render(w, r, http.StatusUnauthorized, "login", &pageData{Error: "Wrong password."})
	default:
		slog.Error("admin ui: sign in", "err", err)
		s.render(w, r, http.StatusInternalServerError, "login", &pageData{Error: "Something went wrong. Try again."})
	}
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := adminauth.Logout(r.Context(), s.db, adminauth.Token(r)); err != nil {
		slog.Error("admin ui: sign out", "err", err)
		s.render(w, r, http.StatusInternalServerError, "login", &pageData{Error: "Something went wrong. Try again."})
		return
	}
	http.SetCookie(w, adminauth.ClearCookie(s.secure(r)))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// home sends the admin to the first page.
func home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}
