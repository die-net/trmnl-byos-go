package renderproxy

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/cache"
	"github.com/die-net/trmnl-byos-go/internal/guard"
)

// origin is an HTTPS server that stands in for every upstream host and
// counts the requests it gets by host and path.
type origin struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
	fail bool
}

func newOrigin(t *testing.T) *origin {
	t.Helper()
	o := &origin{hits: map[string]int{}}
	o.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.hits[r.Host+r.URL.Path]++
		fail := o.fail
		o.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch {
		case r.Host == "fonts.googleapis.com":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			_, _ = io.WriteString(w, "@font-face { src: url(https://fonts.gstatic.com/s/inter/v1/a.woff2); }")
		case strings.HasPrefix(r.URL.Path, "/images/"):
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = io.WriteString(w, "<svg/>")
		case r.URL.Path == "/private":
			w.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(w, "secret")
		default:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "asset")
		}
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *origin) count(hostPath string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hits[hostPath]
}

func (o *origin) setFail(fail bool) {
	o.mu.Lock()
	o.fail = fail
	o.mu.Unlock()
}

// toOrigin is a transport that sends every request, for any host, to the
// test origin, trusting its certificate.
func toOrigin(o *origin) *http.Transport {
	addr := o.Listener.Addr().String()
	var d net.Dialer
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // G402: the test origin's certificate is self-signed.
	}
}

type harness struct {
	origin *origin
	proxy  *Server
	client *http.Client
	ct     *cachingTransport
}

func newHarness(t *testing.T, dialer ContextDialer) *harness {
	t.Helper()
	o := newOrigin(t)
	c, err := cache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := newServer(Config{
		Dialer:      dialer,
		Cache:       c,
		CachedHosts: []string{"trmnl.com", "fonts.googleapis.com", "fonts.gstatic.com"},
	}, toOrigin(o))
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	proxyURL, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // G402: the test origin's certificate is self-signed.
	}}
	ct, _ := p.rp.Transport.(*cachingTransport)
	return &harness{origin: o, proxy: p, client: client, ct: ct}
}

func (h *harness) get(t *testing.T, rawURL, agent string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if agent != "" {
		req.Header.Set("User-Agent", agent)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body), resp.Header
}

// TestCachesVersionedAsset checks that a plain-HTTP request for a cached
// host is fetched once over HTTPS and then served from the cache, with a
// CORS header either way.
func TestCachesVersionedAsset(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &net.Dialer{})
	for range 3 {
		code, body, hdr := h.get(t, "http://trmnl.com/css/3.4.6/plugins.css", "")
		if code != http.StatusOK || body != "asset" || hdr.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("got %d %q %v", code, body, hdr)
		}
	}
	if n := h.origin.count("trmnl.com/css/3.4.6/plugins.css"); n != 1 {
		t.Fatalf("origin fetched %d times, want 1", n)
	}

	// A versioned path never goes stale, and using it keeps it through a
	// sweep of entries unused for a month.
	later := time.Now().Add(365 * 24 * time.Hour)
	h.ct.now = func() time.Time { return later }
	h.get(t, "http://trmnl.com/css/3.4.6/plugins.css", "")
	if n := h.origin.count("trmnl.com/css/3.4.6/plugins.css"); n != 1 {
		t.Fatalf("versioned asset refetched after a year: %d fetches", n)
	}
	if removed, err := h.ct.cache.Sweep(later, 30*24*time.Hour, 0); err != nil || removed != 0 {
		t.Fatalf("sweep removed %d entries, %v; want the one in use kept", removed, err)
	}
	if removed, err := h.ct.cache.Sweep(later.Add(31*24*time.Hour), 30*24*time.Hour, 0); err != nil || removed != 1 {
		t.Fatalf("sweep a month on removed %d entries, %v; want the unused one gone", removed, err)
	}
}

// TestFreshnessAndStale checks that an unversioned asset is refetched once
// its max-age passes, and that a stale copy is served while the origin
// fails.
func TestFreshnessAndStale(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &net.Dialer{})
	const u = "http://trmnl.com/images/plugins/weather/wi-rain.svg"
	h.get(t, u, "")
	h.get(t, u, "")
	if n := h.origin.count("trmnl.com/images/plugins/weather/wi-rain.svg"); n != 1 {
		t.Fatalf("fresh asset fetched %d times, want 1", n)
	}
	h.ct.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	h.get(t, u, "")
	if n := h.origin.count("trmnl.com/images/plugins/weather/wi-rain.svg"); n != 2 {
		t.Fatalf("stale asset fetched %d times in all, want 2", n)
	}

	h.ct.now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	h.origin.setFail(true)
	code, body, _ := h.get(t, u, "")
	if code != http.StatusOK || body != "<svg/>" {
		t.Fatalf("with the origin down: %d %q, want the stale copy", code, body)
	}
}

// TestGoogleFonts checks that a Google Fonts stylesheet is cached per
// User-Agent and has its font URLs pointed at plain HTTP.
func TestGoogleFonts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &net.Dialer{})
	const u = "http://fonts.googleapis.com/css2?family=Inter"
	for _, agent := range []string{"firefox", "firefox", "other"} {
		code, body, _ := h.get(t, u, agent)
		if code != http.StatusOK || !strings.Contains(body, "url(http://fonts.gstatic.com/s/inter/v1/a.woff2)") {
			t.Fatalf("%s: %d %q", agent, code, body)
		}
	}
	if n := h.origin.count("fonts.googleapis.com/css2"); n != 2 {
		t.Fatalf("stylesheet fetched %d times, want once per agent: 2", n)
	}
}

// TestNotCached checks that responses that must not be stored, and hosts
// not on the list, go to the origin every time.
func TestNotCached(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &net.Dialer{})
	for range 2 {
		if code, body, _ := h.get(t, "http://trmnl.com/private", ""); code != http.StatusOK || body != "secret" {
			t.Fatalf("no-store: %d %q", code, body)
		}
	}
	if n := h.origin.count("trmnl.com/private"); n != 2 {
		t.Fatalf("no-store fetched %d times, want 2", n)
	}
}

// TestRefusesNonProxyRequest checks that a request not in proxy form is
// refused rather than sent to a guessed origin.
func TestRefusesNonProxyRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &net.Dialer{})
	rec := httptest.NewRecorder()
	h.proxy.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/css/3.4.6/plugins.css", http.NoBody))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

// TestConnect checks that a CONNECT tunnel reaches an allowed origin, and
// that the guard dialer refuses one to a private address.
func TestConnect(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &net.Dialer{})
	code, body, _ := h.get(t, h.origin.URL+"/tunneled", "")
	if code != http.StatusOK || body != "asset" || h.origin.count(strings.TrimPrefix(h.origin.URL, "https://")+"/tunneled") != 1 {
		t.Fatalf("through the tunnel: %d %q", code, body)
	}

	guarded := newHarness(t, guard.NewDialer(5*time.Second))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, guarded.origin.URL+"/tunneled", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := guarded.client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("tunneled to a loopback origin through the guard dialer")
	}
	if !strings.Contains(err.Error(), "Bad Gateway") {
		t.Fatalf("refused with %v, want the proxy's 502", err)
	}
	if n := guarded.origin.count(strings.TrimPrefix(guarded.origin.URL, "https://") + "/tunneled"); n != 0 {
		t.Fatalf("the private origin got %d requests", n)
	}
}

// TestGuardedPlainHTTP checks that the guard dialer refuses a plain-HTTP
// request to a private address too.
func TestGuardedPlainHTTP(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	c, err := cache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Dialer: guard.NewDialer(5 * time.Second), Cache: c})
	rec := httptest.NewRecorder()
	target := strings.Replace(o.URL, "https://", "http://", 1) + "/x"
	p.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", rec.Code)
	}
}
