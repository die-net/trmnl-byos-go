package render

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/cache"
	"github.com/die-net/trmnl-byos-go/internal/renderproxy"
)

// startTestProxy serves the real render proxy, which may dial loopback, on a
// new loopback listener through Sandbox.serveProxy, and returns the
// listener's address and the func that stops it. It is also stopped when the
// test ends.
func startTestProxy(t *testing.T) (addr string, stop func()) {
	t.Helper()
	c, err := cache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proxy := renderproxy.New(renderproxy.Config{Dialer: &net.Dialer{}, Cache: c})
	sb, _ := newUnitSandbox(t, SandboxConfig{Proxy: proxy})
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop = sb.serveProxy(ln)
	t.Cleanup(stop)
	return ln.Addr().String(), stop
}

// dialTest opens a TCP connection to addr, closed when the test ends.
func dialTest(t *testing.T, addr string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// isTimeout reports whether err is a deadline expiring, not a connection
// ending.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout() || errors.Is(err, os.ErrDeadlineExceeded)
}

// TestServeProxyStopEndsTunnels checks that stopping a build's proxy closes
// both sides of a CONNECT tunnel that is open and idle, so a tunnel does not
// outlive its build and hold a descriptor of the server's.
func TestServeProxyStopEndsTunnels(t *testing.T) {
	t.Parallel()
	addr, stop := startTestProxy(t)

	// The upstream accepts and then neither writes nor closes.
	upstream, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := upstream.Accept(); err == nil {
			accepted <- c
		}
	}()

	client := dialTest(t, addr)
	target := upstream.Addr().String()
	if _, err := client.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	status, err := bufio.NewReader(client).ReadString('\n')
	if err != nil || !strings.Contains(status, " 200") {
		t.Fatalf("CONNECT answered %q, %v; want 200", status, err)
	}
	var up net.Conn
	select {
	case up = <-accepted:
		defer up.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never got the tunnel's connection")
	}

	stop()

	for name, conn := range map[string]net.Conn{"client": client, "upstream": up} {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil || isTimeout(err) {
			t.Errorf("%s side after stop: read %d bytes, %v; want the connection closed", name, n, err)
		}
	}
}

// TestServeProxyConnectionCap checks that a build cannot hold more than
// maxProxyConns connections to its proxy: one more is not served while they
// are all open, and is as soon as one closes. The proxy runs in the server's
// process, so this bounds the descriptors a build can use up.
func TestServeProxyConnectionCap(t *testing.T) {
	t.Parallel()
	addr, _ := startTestProxy(t)

	idle := make([]net.Conn, maxProxyConns)
	for i := range idle {
		idle[i] = dialTest(t, addr)
	}
	// A partial request header keeps each connection open on the server's
	// side, which holds its slot until the client closes it.
	for _, c := range idle {
		if _, err := c.Write([]byte("CONNECT 127.0.0.1:1 HTTP/1.1\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	extra := dialTest(t, addr)
	const request = "CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n"
	if _, err := extra.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(extra)
	_ = extra.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if line, err := r.ReadString('\n'); err == nil || !isTimeout(err) {
		t.Fatalf("a connection over the cap got %q, %v; want no answer", line, err)
	}

	if err := idle[0].Close(); err != nil {
		t.Fatal(err)
	}
	_ = extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := r.ReadString('\n')
	if err != nil || !strings.Contains(line, " 502") {
		t.Errorf("after one closed, the extra connection got %q, %v; want a 502", line, err)
	}
}
