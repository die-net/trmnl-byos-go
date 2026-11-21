package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
)

// TestRunProxy checks the proxy command's two listeners: the forward sends
// the server's requests to the renderer, the proxy refuses the renderer's
// requests to a private address, such as the renderer itself, and both stop
// when the context ends.
func TestRunProxy(t *testing.T) {
	t.Parallel()
	renderer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "renderer")
	}))
	defer renderer.Close()
	target := renderer.Listener.Addr().String()

	pln, err := listenOn(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fln, err := listenOn(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runProxy(ctx, pln, t.TempDir(), fln, target) }()

	get := func(client *http.Client, u string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}

	direct := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
	if code, body := get(direct, "http://"+fln.Addr().String()+"/build"); code != http.StatusOK || body != "renderer" {
		t.Errorf("through the forward: %d %q, want the renderer's answer", code, body)
	}
	proxied := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: pln.Addr().String()})}, Timeout: 10 * time.Second}
	if code, body := get(proxied, "http://"+target+"/build"); code != http.StatusBadGateway {
		t.Errorf("through the proxy to a private address: %d %q, want it refused", code, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runProxy after its context ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runProxy did not return after its context ended")
	}
}

// TestProxyFlags checks that the proxy command refuses bad flags at
// startup: a forward without both its flags, a forward target with no port,
// and an empty listen address. Each case listens only on loopback, and a
// deadline ends a run that was not refused, so a check that stops refusing
// fails rather than hangs.
func TestProxyFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"forward listen alone", []string{"-forward-listen", "127.0.0.1:0"}},
		{"forward target alone", []string{"-forward-to", "renderer:8081"}},
		{"forward target with no port", []string{"-forward-listen", "127.0.0.1:0", "-forward-to", "renderer"}},
		{"forward target with an empty port", []string{"-forward-listen", "127.0.0.1:0", "-forward-to", "renderer:"}},
		{"empty listen", []string{"-listen", ""}},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		args := tc.args
		if args[0] != "-listen" {
			args = append([]string{"-listen", "127.0.0.1:0", "-cache", t.TempDir()}, args...)
		}
		if err := proxy(ctx, args); err == nil {
			t.Errorf("%s: no error, want one", tc.name)
		}
		cancel()
	}
}

// TestServeWithoutProxy checks serve with -render-url and no -proxy-listen,
// as Compose runs it: the scheduler still sends builds to the renderer, and
// no proxy is started, so its cache directory is never made.
func TestServeWithoutProxy(t *testing.T) {
	t.Parallel()
	builds := make(chan struct{}, 1)
	renderer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case builds <- struct{}{}:
		default:
		}
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer renderer.Close()

	// A recipe on a device's playlist, so the scheduler has a build to send.
	dir := dataDir(t)
	client, err := openDB(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	writeRecipeTo(t, src, "name: Hourly\nrefresh_interval: 60\n")
	rec, err := recipe.Install(t.Context(), client, recipesDir(dir), src, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	inst, err := client.Write.CreateInstance(t.Context(), db.CreateInstanceParams{RecipeID: rec.ID, Name: "hourly", FieldValues: "{}", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := client.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
		Mac: "D8:3B:DA:12:34:56", ApiKeyHash: []byte{1}, FriendlyID: "ABC123", FwModel: "xiao_epaper_display", CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := addItemTo(t.Context(), client, "", dev.FriendlyID, inst.ID, playlist.DefaultDuration); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	cacheDir := filepath.Join(t.TempDir(), "cache")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, []string{"-data", dir, "-listen", "127.0.0.1:0", "-render-url", renderer.URL, "-cache", cacheDir})
	}()
	select {
	case <-builds:
	case err := <-done:
		t.Fatalf("serve returned before a build: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the renderer got no build")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve after its context ended: %v", err)
	}
	if _, err := os.Stat(cacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cache directory: %v, want none, since no proxy runs", err)
	}
}

// TestStartProxyClosesOnFailure checks that a proxy whose cache cannot be
// made closes its listener, so nothing is left listening.
func TestStartProxyClosesOnFailure(t *testing.T) {
	t.Parallel()
	ln, err := listenOn(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fln, err := listenOn(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := runProxy(t.Context(), ln, "\x00", fln, "renderer:8081"); err == nil {
		t.Fatal("runProxy with a bad cache directory: no error")
	}
	for _, l := range []net.Listener{ln, fln} {
		if _, err := l.Accept(); err == nil {
			t.Errorf("%s still accepts", l.Addr())
		}
	}
}
