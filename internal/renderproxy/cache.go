package renderproxy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/cache"
)

// defaultMaxAge is how long an asset with no Cache-Control max-age stays
// fresh.
const defaultMaxAge = time.Hour

// immutablePath matches asset paths that include a version, whose content
// never changes, so a cached copy is kept without asking again.
var immutablePath = map[string]*regexp.Regexp{
	"trmnl.com":           regexp.MustCompile(`^/(css|js)/\d+\.\d+\.\d+/|^/js/[a-z-]+/\d+\.\d+\.\d+/`),
	"code.highcharts.com": regexp.MustCompile(`^/\d+\.\d+\.\d+/`),
	// Google's font files live at content-addressed, versioned paths.
	"fonts.gstatic.com": regexp.MustCompile(`^/s/`),
}

// entryHeader is the first line of a cached asset: the request fields that
// make up its key, and the response to replay.
type entryHeader struct {
	URL string `json:"url"`
	// UserAgent is set for a response that varies by it, such as a Google
	// Fonts stylesheet, which names a font format the browser takes.
	UserAgent string      `json:"user_agent,omitempty"`
	Status    int         `json:"status"`
	Header    http.Header `json:"header"`
	// Immutable is set for a versioned path, which never goes stale.
	Immutable bool `json:"immutable,omitempty"`
	// MaxAge is how long the entry stays fresh after it was stored.
	MaxAge time.Duration `json:"max_age"`
}

// cachingTransport answers GETs for cached hosts from the cache, and stores
// what it fetches for them as it streams to the client.
type cachingTransport struct {
	base  http.RoundTripper
	cache *cache.Cache
	hosts map[string]bool
	now   func() time.Time
}

// variesByAgent reports whether a host's responses differ by User-Agent.
func variesByAgent(host string) bool {
	return host == "fonts.googleapis.com"
}

func (t *cachingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := hostOf(req)
	if req.Method != http.MethodGet || req.URL.Scheme != "https" || !t.hosts[host] || t.cache == nil {
		return t.base.RoundTrip(req)
	}
	h := entryHeader{URL: req.URL.String()}
	if variesByAgent(host) {
		h.UserAgent = req.Header.Get("User-Agent")
	}
	key := cache.KeyOf("asset", h.URL, h.UserAgent)

	var cached entryHeader
	entry, err := t.cache.Open(key, &cached)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("render proxy: read cache", "url", h.URL, "err", err)
	}
	if entry != nil {
		if cached.Immutable {
			// An immutable entry's time is when it was last used, at most
			// an hour off, so the cache janitor keeps the ones in use.
			if now := t.now(); now.Sub(entry.ModTime) > time.Hour {
				if err := t.cache.Touch(key, now); err != nil {
					slog.Warn("render proxy: touch cache entry", "url", h.URL, "err", err)
				}
			}
			return replay(req, cached, entry), nil
		}
		if t.now().Before(entry.ModTime.Add(cached.MaxAge)) {
			return replay(req, cached, entry), nil
		}
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode >= http.StatusInternalServerError {
		if entry != nil {
			// The origin is down: a stale copy beats a broken render.
			if resp != nil {
				_ = resp.Body.Close()
			}
			slog.Info("render proxy: serving stale asset", "url", h.URL, "err", err)
			return replay(req, cached, entry), nil
		}
		return resp, err
	}
	if entry != nil {
		_ = entry.Close()
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	maxAge, store := freshness(resp.Header.Get("Cache-Control"))
	if !store {
		return resp, nil
	}
	h.Status = resp.StatusCode
	h.Header = storedHeader(resp.Header)
	h.MaxAge = maxAge
	if re := immutablePath[host]; re != nil && re.MatchString(req.URL.Path) {
		h.Immutable = true
	}
	w, err := t.cache.Create(key, h)
	if err != nil {
		slog.Warn("render proxy: write cache", "url", h.URL, "err", err)
		return resp, nil
	}
	resp.Body = &teeBody{src: resp.Body, w: w, url: h.URL}
	return rewrite(resp, host), nil
}

// replay answers req from a cached entry, streaming its body from disk.
func replay(req *http.Request, h entryHeader, e *cache.Entry) *http.Response {
	resp := &http.Response{
		Status:        fmt.Sprintf("%d %s", h.Status, http.StatusText(h.Status)),
		StatusCode:    h.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h.Header.Clone(),
		Body:          e,
		ContentLength: e.Size,
		Request:       req,
	}
	return rewrite(resp, hostOf(req))
}

// rewrite adjusts a cached host's response before it reaches the browser.
//
// The page that loads these assets comes from trmnlp's own local server, so
// fonts and scripts are cross-origin to it, and the browser needs a CORS
// header. trmnl.com sends one only to a request with an Origin, and a
// cached response may have been stored from one without, so the header is
// always set; these are public static files.
//
// A Google Fonts stylesheet names its font files on fonts.gstatic.com over
// HTTPS; they are pointed at plain HTTP so the browser fetches them through
// the cache too. The stylesheet is a few kilobytes, so it is rewritten
// whole, within a bound.
func rewrite(resp *http.Response, host string) *http.Response {
	resp.Header.Set("Access-Control-Allow-Origin", "*")
	if host != "fonts.googleapis.com" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
		return resp
	}
	resp.Body = &replaceBody{src: resp.Body, old: "https://fonts.gstatic.com/", new: "http://fonts.gstatic.com/"}
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return resp
}

// maxStylesheet bounds a stylesheet rewrite reads whole.
const maxStylesheet = 1 << 20

// replaceBody replaces old with new in a small body, read whole on the
// first Read.
type replaceBody struct {
	src      io.ReadCloser
	old, new string
	out      *strings.Reader
	err      error
}

func (b *replaceBody) Read(p []byte) (int, error) {
	if b.out == nil && b.err == nil {
		data, err := io.ReadAll(io.LimitReader(b.src, maxStylesheet+1))
		switch {
		case err != nil:
			b.err = err
		case len(data) > maxStylesheet:
			b.err = errors.New("stylesheet too large to rewrite")
		default:
			b.out = strings.NewReader(strings.ReplaceAll(string(data), b.old, b.new))
		}
	}
	if b.err != nil {
		return 0, b.err
	}
	return b.out.Read(p)
}

func (b *replaceBody) Close() error { return b.src.Close() }

// teeBody streams a response body to the client and into a cache entry,
// committing the entry only when the body was read to its end.
type teeBody struct {
	src  io.ReadCloser
	w    *cache.Writer
	url  string
	done bool
}

func (b *teeBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	if n > 0 && !b.done {
		if _, werr := b.w.Write(p[:n]); werr != nil {
			slog.Warn("render proxy: write cache", "url", b.url, "err", werr)
			b.w.Abort()
			b.done = true
		}
	}
	if errors.Is(err, io.EOF) && !b.done {
		b.done = true
		if cerr := b.w.Commit(); cerr != nil {
			slog.Warn("render proxy: commit cache", "url", b.url, "err", cerr)
		}
	}
	return n, err
}

// Close discards an entry whose body was not read to the end.
func (b *teeBody) Close() error {
	if !b.done {
		b.done = true
		b.w.Abort()
	}
	return b.src.Close()
}

// freshness reads a Cache-Control header: how long a response stays fresh,
// and whether it may be stored at all.
func freshness(cc string) (time.Duration, bool) {
	maxAge := defaultMaxAge
	for part := range strings.SplitSeq(cc, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(strings.ToLower(part)), "=")
		switch name {
		case "no-store", "private":
			return 0, false
		case "no-cache":
			maxAge = 0
		case "max-age":
			if s, err := strconv.ParseInt(strings.Trim(value, `"`), 10, 64); err == nil && s >= 0 {
				maxAge = time.Duration(s) * time.Second
			}
		}
	}
	return maxAge, true
}

// storedHeader is the part of a response's header worth replaying.
func storedHeader(h http.Header) http.Header {
	out := http.Header{}
	for _, name := range []string{"Content-Type", "Cache-Control", "Last-Modified", "ETag"} {
		if v := h.Values(name); len(v) > 0 {
			out[name] = v
		}
	}
	return out
}
