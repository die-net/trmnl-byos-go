package adminui

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
)

func TestRelativeTime(t *testing.T) {
	for _, tc := range []struct {
		ago  time.Duration
		want string
	}{
		{-time.Hour, "Just now"},
		{0, "Just now"},
		{59 * time.Second, "Just now"},
		{time.Minute, "1 min ago"},
		{4*time.Minute + 59*time.Second, "4 min ago"},
		{59*time.Minute + 59*time.Second, "59 min ago"},
		{time.Hour, "1 h ago"},
		{2*time.Hour + 30*time.Minute, "2 h ago"},
		{23*time.Hour + 59*time.Minute, "23 h ago"},
		{24 * time.Hour, "1 d ago"},
		{3*24*time.Hour + time.Hour, "3 d ago"},
	} {
		if got := relativeTime(tc.ago); got != tc.want {
			t.Errorf("relativeTime(%v) = %q, want %q", tc.ago, got, tc.want)
		}
	}
}

// addDevice registers a device, and records a wake if lastSeen is not 0.
func addDevice(t *testing.T, tu *testUI, mac, friendly, name string, lastSeen int64) {
	t.Helper()
	ctx := t.Context()
	dev, err := tu.db.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: mac, ApiKeyHash: []byte(mac), FriendlyID: friendly, Name: name, CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if lastSeen == 0 {
		return
	}
	volts, rssi := 4.054, int64(-61)
	err = tu.db.Write.UpsertDeviceState(ctx, db.UpsertDeviceStateParams{DeviceID: dev.ID, LastSeen: lastSeen, BatteryV: &volts, Rssi: &rssi, LastRefreshRateSent: 900})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDevicesBadges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ago      time.Duration // 0 means never seen
		badge    string
		class    string
		lastSeen string
	}{
		{"never seen", 0, "Never seen", "badge-neutral", "Never"},
		{"online", 4 * time.Minute, "Online", "badge-ok", "4 min ago"},
		{"online at twice the interval", 30 * time.Minute, "Online", "badge-ok", "30 min ago"},
		{"late", 2 * time.Hour, "Late", "badge-warn", "2 h ago"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tu := newTestUI(t, Config{}, true)
			var seen int64
			if tc.ago != 0 {
				seen = tu.now.Add(-tc.ago).Unix()
			}
			addDevice(t, tu, "D8:3B:DA:00:00:01", "ABC123", "Kitchen", seen)
			c := tu.signIn(t)
			w := tu.do(t, "GET", "/devices", "", "Cookie", cookieHeader(c))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			body := w.Body.String()
			for _, want := range []string{
				`<h2><a class="device-link" href="/devices/ABC123">Kitchen</a></h2>`,
				`<span class="badge ` + tc.class + `">` + tc.badge + `</span>`,
				">" + tc.lastSeen + "<",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
			if tc.ago != 0 {
				for _, want := range []string{"4.05 V", "-61 dBm", `<time datetime="`} {
					if !strings.Contains(body, want) {
						t.Errorf("page lacks %q", want)
					}
				}
			} else if strings.Contains(body, "<time") || strings.Count(body, "–") < 3 {
				t.Errorf("a device never seen should show no time and placeholders:\n%s", body)
			}
		})
	}
}

func TestDevicesNameFallbackAndEscaping(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, "D8:3B:DA:00:00:01", "ABC123", "<script>alert(1)</script>", 0)
	addDevice(t, tu, "D8:3B:DA:00:00:02", "XYZ789", "", 0)
	c := tu.signIn(t)
	body := tu.do(t, "GET", "/devices", "", "Cookie", cookieHeader(c)).Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("a device name was not escaped")
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", `<h2><a class="device-link" href="/devices/XYZ789">XYZ789</a></h2>`, `<p class="detail">ABC123</p>`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// A device with no name is headed by its friendly ID, which the line
	// under the heading then leaves out.
	if n := strings.Count(body, ">XYZ789<"); n != 1 {
		t.Errorf("the friendly ID of an unnamed device is shown %d times, want 1", n)
	}
}

func TestDevicesEmpty(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	c := tu.signIn(t)
	body := tu.do(t, "GET", "/devices", "", "Cookie", cookieHeader(c)).Body.String()
	for _, want := range []string{"<h2>No devices yet</h2>", "Open for 10 minutes"} {
		if !strings.Contains(body, want) {
			t.Errorf("empty page lacks %q", want)
		}
	}
	if strings.Contains(body, `class="grid"`) {
		t.Error("empty page has a device grid")
	}
}

func TestNavCurrent(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	c := tu.signIn(t)
	current := regexp.MustCompile(`<a href="(/[a-z]+)" aria-current="page">`)
	for _, path := range []string{"/devices", "/recipes", "/playlists", "/firmware"} {
		t.Run(path, func(t *testing.T) {
			w := tu.do(t, "GET", path, "", "Cookie", cookieHeader(c))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			body := w.Body.String()
			if m := current.FindAllStringSubmatch(body, -1); len(m) != 1 || m[0][1] != path {
				t.Errorf("current nav items = %v, want only %s", m, path)
			}
			for _, want := range []string{`<nav class="nav" aria-label="Main">`, `href="#main"`, `id="main"`} {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q", want)
				}
			}
			if n := strings.Count(body, "<h1>"); n != 1 {
				t.Errorf("%d h1 elements, want 1", n)
			}
			for _, bad := range []string{"<script", "style=", "<style"} {
				if strings.Contains(body, bad) {
					t.Errorf("page has inline %q, which the CSP refuses", bad)
				}
			}
		})
	}
}

func TestStubPages(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	c := tu.signIn(t)
	for _, tc := range []struct{ path, heading string }{
		{"/recipes", "Recipes"}, {"/playlists", "Playlists"}, {"/firmware", "Firmware"},
	} {
		body := tu.do(t, "GET", tc.path, "", "Cookie", cookieHeader(c)).Body.String()
		for _, want := range []string{"<h1>" + tc.heading + "</h1>", "This page is not built yet."} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", tc.path, want)
			}
		}
	}
}
