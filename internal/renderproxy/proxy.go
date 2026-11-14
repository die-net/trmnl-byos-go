// Package renderproxy is the forward proxy the renderer's builds go through.
// The renderer runs on a network with no other way out, so this proxy is
// where its connections are checked: every one goes through a dialer that
// reaches only public addresses. It also caches the assets recipes load from
// a few hosts, which builds request over plain HTTP so the proxy can see and
// cache them, fetching them upstream over HTTPS.
//
// The proxy server and its bidirectional copy are adapted from conduit's
// internal/proxy and internal/conn (github.com/die-net/conduit, 9a21684).
package renderproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/die-net/trmnl-byos-go/internal/cache"
)

// ContextDialer makes outbound connections. guard.Dialer is the one to use.
type ContextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Config is how a proxy is set up.
type Config struct {
	// Dialer makes every outbound connection, for tunnels and for proxied
	// requests alike.
	Dialer ContextDialer
	// Cache holds cached assets.
	Cache *cache.Cache
	// CachedHosts are the hosts whose assets are cached. A plain-HTTP
	// request to one is fetched upstream over HTTPS.
	CachedHosts []string
}

// AssetHosts are the hosts whose assets recipes load and the proxy caches:
// the TRMNL framework, Google Fonts, and Highcharts. A build's .trmnlp.yml
// sends their requests over plain HTTP so the proxy can see them.
var AssetHosts = []string{"trmnl.com", "fonts.googleapis.com", "fonts.gstatic.com", "code.highcharts.com"}

// Server is the proxy.
type Server struct {
	dialer ContextDialer
	rp     *httputil.ReverseProxy
}

// New returns a proxy.
func New(cfg Config) *Server {
	return newServer(cfg, newTransport(cfg.Dialer))
}

// newServer returns a proxy that sends upstream requests through base. Tests
// use it to point the proxy at a local origin.
func newServer(cfg Config, base http.RoundTripper) *Server {
	hosts := make(map[string]bool, len(cfg.CachedHosts))
	for _, h := range cfg.CachedHosts {
		hosts[h] = true
	}
	ct := &cachingTransport{base: base, cache: cfg.Cache, hosts: hosts, now: time.Now}
	return &Server{dialer: cfg.Dialer, rp: newReverseProxy(ct, hosts)}
}

// ServeHTTP handles one proxy request: a CONNECT tunnel, or a plain-HTTP
// request in absolute form.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	if !r.URL.IsAbs() {
		// Not a proxy request; refuse rather than guess an origin.
		http.Error(w, "proxy requests only", http.StatusBadRequest)
		return
	}
	s.rp.ServeHTTP(w, r)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}
	_ = brw.Flush()

	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	ctx := r.Context()
	serverConn, err := s.dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		slog.Info("render proxy: refused or failed tunnel", "target", target, "err", err)
		_, _ = writeError(brw, err, http.StatusBadGateway)
		_ = brw.Flush()
		_ = clientConn.Close()
		return
	}
	slog.Debug("render proxy: tunnel", "target", target)
	_, _ = brw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = brw.Flush()
	_ = copyBidirectional(ctx, clientConn, serverConn)
}

// writeError writes an HTTP error on a hijacked connection.
func writeError(brw *bufio.ReadWriter, err error, code int) (int, error) {
	return fmt.Fprintf(brw, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n%s\r\n",
		code, http.StatusText(code), err.Error())
}

func newReverseProxy(rt http.RoundTripper, cached map[string]bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := *pr.In.URL
			if cached[u.Hostname()] && u.Scheme == "http" {
				// Builds ask for these over plain HTTP so the proxy can
				// cache them; the origin is reached over HTTPS.
				u.Scheme = "https"
				if u.Port() == "80" {
					u.Host = u.Hostname()
				}
				// Cached bodies are stored as sent, so ask for them
				// uncompressed; Firefox compresses nothing it sends here.
				pr.Out.Header.Del("Accept-Encoding")
			}
			pr.Out.URL = &u
			pr.Out.Host = u.Host
		},
		Transport:     rt,
		FlushInterval: 10 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Info("render proxy: request failed", "host", r.URL.Hostname(), "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
		},
		BufferPool: newBufferPool(32 << 10),
	}
}

func newTransport(d ContextDialer) *http.Transport {
	return &http.Transport{
		// Never another proxy: the dialer's check must see the origin's
		// address.
		Proxy:                 nil,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(0),
		},
	}
}

// copyBidirectional copies between left and right until one side fails or
// ctx is done. Each side is half-closed when its copy ends, where the
// connection allows. It sets no deadlines, so io.Copy can use the
// zero-copy path where there is one.
func copyBidirectional(ctx context.Context, left, right io.ReadWriteCloser) error {
	g, gctx := errgroup.WithContext(ctx)
	context.AfterFunc(gctx, func() {
		_ = left.Close()
		_ = right.Close()
	})
	g.Go(func() error { return copyClose(left, right) })
	g.Go(func() error { return copyClose(right, left) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// copyClose copies src to dst, then half-closes dst, or closes it.
func copyClose(dst, src io.ReadWriteCloser) error {
	_, err := io.Copy(dst, src)
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return err
	}
	_ = dst.Close()
	return err
}

// bufferPool is an httputil.BufferPool of fixed-size buffers.
type bufferPool struct {
	pool sync.Pool
}

func newBufferPool(size int) *bufferPool {
	bp := &bufferPool{}
	bp.pool.New = func() any {
		b := make([]byte, size)
		return &b
	}
	return bp
}

func (p *bufferPool) Get() []byte {
	b, _ := p.pool.Get().(*[]byte)
	return *b
}

func (p *bufferPool) Put(b []byte) {
	p.pool.Put(&b)
}

// hostOf is the host part of r's URL, lowercased.
func hostOf(r *http.Request) string {
	return strings.ToLower(r.URL.Hostname())
}
