// Package deviceapi serves the endpoints TRMNL firmware calls: /api/setup,
// /api/display and /api/log, plus the images /api/display points at.
//
// The firmware reads a JSON status field, not the HTTP status, so /api/setup
// and /api/display answer HTTP 200 with the outcome in the body.
package deviceapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
)

// Statuses in a response's JSON status field, as the firmware reads them.
const (
	// statusOK is what /api/display sends for a normal response. The
	// firmware ignores a response whose status is 200.
	statusOK = 0
	// statusSetupOK is what /api/setup sends for a registered device.
	statusSetupOK = 200
	// statusNotRegistered tells /api/display the device is unknown. The
	// device keeps its credentials and Wi-Fi settings, shows its own setup
	// screen, and polls again soon. Status 500 would wipe both, the same as
	// reset_firmware, so it is never sent.
	statusNotRegistered = 202
	// statusSetupUnknown tells /api/setup the MAC is not registered. The
	// device shows the response's image and tries again in 5 minutes.
	statusSetupUnknown = 404
)

// Config is how the device API is set up.
type Config struct {
	// PublicURL is the base URL devices reach this server at, such as
	// http://192.168.1.10:8080. Empty uses the scheme and Host of each
	// request.
	PublicURL string
	// RefreshRate is how long a device sleeps between wakes.
	RefreshRate time.Duration
	// FirmwareDir is where firmware files are stored. Empty offers and
	// serves no firmware.
	FirmwareDir string
}

// Server handles device requests.
type Server struct {
	db     *db.Client
	images *imagestore.Store
	cfg    Config
	now    func() time.Time

	mu sync.Mutex
	// screens maps a system screen's profile and text to its image name,
	// so a screen is drawn once, not on every wake. It is drawn again only
	// if image expiry deleted it, or if it was dropped to stay within
	// maxScreens.
	screens map[string]string
}

// New returns a Server that keeps state in client and images in images.
func New(client *db.Client, images *imagestore.Store, cfg Config) *Server {
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	return &Server{db: client, images: images, cfg: cfg, now: time.Now, screens: map[string]string{}}
}

// Register adds the device endpoints to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup", s.setup)
	mux.HandleFunc("GET /api/display", s.display)
	mux.HandleFunc("POST /api/log", s.log)
	mux.HandleFunc("GET /firmware/{name}", s.firmware)
	mux.Handle("GET /images/{name}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		debugRequest(r)
		s.images.ServeHTTP(w, r)
	}))
}

// imageURL is the absolute URL a device fetches the named image from. The
// firmware cannot follow a relative one.
func (s *Server) imageURL(r *http.Request, name string) string {
	return s.baseURL(r) + "/images/" + name + ".png"
}

// baseURL is the URL devices reach this server at, with no trailing slash.
func (s *Server) baseURL(r *http.Request) string {
	base := s.cfg.PublicURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return base
}

func headerString(r *http.Request, name string) *string {
	v := r.Header.Get(name)
	if v == "" {
		return nil
	}
	return &v
}

func headerInt(r *http.Request, name string) *int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get(name)), 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

func headerFloat(r *http.Request, name string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(r.Header.Get(name)), 64)
	if err != nil {
		return nil
	}
	return &v
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "err", err)
	}
}

// debugRequest logs a device request's headers at debug level, without its
// Access-Token, so real requests can be captured for tests.
func debugRequest(r *http.Request) {
	if !slog.Default().Enabled(r.Context(), slog.LevelDebug) {
		return
	}
	attrs := make([]any, 0, len(r.Header)+2)
	attrs = append(attrs, "method", r.Method, "path", r.URL.Path)
	for name, vals := range r.Header {
		if name == "Access-Token" {
			continue
		}
		attrs = append(attrs, slog.String(name, strings.Join(vals, ", ")))
	}
	slog.Debug("device request", attrs...)
}
