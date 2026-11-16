package renderproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// echoServer accepts connections on a local address, answers each with
// prefix and then every byte it reads, and counts them.
func echoServer(t *testing.T, prefix string) (string, *atomic.Int32) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := &atomic.Int32{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, prefix)
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String(), accepted
}

// TestForward checks that Forward copies a connection both ways to its one
// target, whatever the client sends, as a proxy request naming another host
// would, and that it returns nil once its context ends.
func TestForward(t *testing.T) {
	t.Parallel()
	target, _ := echoServer(t, "renderer:")
	other, otherAccepted := echoServer(t, "other:")

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Forward(ctx, ln, target, &net.Dialer{Timeout: time.Second}) }()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "CONNECT " + other + " HTTP/1.1\r\nHost: " + other + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	want := "renderer:" + req
	got := make([]byte, len(want))
	if _, err := io.ReadFull(bufio.NewReader(conn), got); err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("forwarded %q, want %q", got, want)
	}
	if n := otherAccepted.Load(); n != 0 {
		t.Errorf("the other host got %d connections, want none", n)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Forward after its context ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Forward did not return after its context ended")
	}
}

// TestForwardTargetDown checks that a connection whose target cannot be
// reached is closed, and that Forward keeps accepting.
func TestForwardTargetDown(t *testing.T) {
	t.Parallel()
	down, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := down.Addr().String()
	_ = down.Close()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = Forward(t.Context(), ln, target, &net.Dialer{Timeout: time.Second}) }()
	for range 2 {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil {
			t.Errorf("read %d bytes from a forward whose target is down, want it closed", n)
		}
		_ = conn.Close()
	}
}

// fakeListener is a net.Listener whose Accept returns, in turn, what is sent
// on accepts: a connection or an error. Close makes Accept fail as a closed
// listener does.
type fakeListener struct {
	accepts chan accepted
	closed  chan struct{}
	once    atomic.Bool
}

// accepted is one result of a fakeListener's Accept.
type accepted struct {
	conn net.Conn
	err  error
}

func newFakeListener() *fakeListener {
	return &fakeListener{accepts: make(chan accepted), closed: make(chan struct{})}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	select {
	case a := <-l.accepts:
		return a.conn, a.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *fakeListener) Close() error {
	if l.once.CompareAndSwap(false, true) {
		close(l.closed)
	}
	return nil
}

func (*fakeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// forwardPipe hands the forward behind l one end of a pipe and returns the
// other, once a line through it has come back from the target, so the pair
// is up.
func forwardPipe(t *testing.T, l *fakeListener) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	l.accepts <- accepted{conn: server}
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(client, "ping"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("renderer:ping"))
	if _, err := io.ReadFull(client, got); err != nil || string(got) != "renderer:ping" {
		t.Fatalf("through the forward: %q, %v", got, err)
	}
	return client
}

// TestForwardAcceptFails checks that an Accept error that is not temporary
// ends Forward at once, while its context is still live, with the error, and
// that it closes the listener and every connection it forwarded first.
func TestForwardAcceptFails(t *testing.T) {
	t.Parallel()
	target, _ := echoServer(t, "renderer:")
	l := newFakeListener()
	done := make(chan error, 1)
	go func() { done <- Forward(t.Context(), l, target, &net.Dialer{Timeout: time.Second}) }()
	client := forwardPipe(t, l)
	defer client.Close()

	boom := errors.New("boom")
	l.accepts <- accepted{err: boom}
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Errorf("Forward: %v, want the Accept error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Forward did not return after a fatal Accept error")
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Error("a forwarded connection is still open after Forward returned")
	}
	if !l.once.Load() {
		t.Error("the listener is still open after Forward returned")
	}
}

// TestForwardAcceptRetries checks that a temporary Accept error, as running
// out of file descriptors gives, is retried, and that the forward keeps
// serving after it.
func TestForwardAcceptRetries(t *testing.T) {
	t.Parallel()
	target, _ := echoServer(t, "renderer:")
	l := newFakeListener()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Forward(ctx, l, target, &net.Dialer{Timeout: time.Second}) }()

	emfile := &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	for range 3 {
		select {
		case l.accepts <- accepted{err: emfile}:
		case err := <-done:
			t.Fatalf("Forward returned on a temporary error: %v", err)
		}
	}
	client := forwardPipe(t, l)
	_ = client.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Forward after its context ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Forward did not return after its context ended")
	}
}

// TestTemporary checks which Accept errors are retried: the ones net/http's
// Server.Serve retries.
func TestTemporary(t *testing.T) {
	t.Parallel()
	accept := func(errno syscall.Errno) error {
		return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", errno)}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"EMFILE", accept(syscall.EMFILE), true},
		{"ENFILE", accept(syscall.ENFILE), true},
		{"ECONNABORTED", &net.OpError{Op: "accept", Net: "tcp", Err: syscall.ECONNABORTED}, true},
		{"EINVAL", accept(syscall.EINVAL), false},
		{"closed", net.ErrClosed, false},
		{"plain", errors.New("boom"), false},
	} {
		if got := temporary(tc.err); got != tc.want {
			t.Errorf("%s: temporary %v, want %v", tc.name, got, tc.want)
		}
	}
}
