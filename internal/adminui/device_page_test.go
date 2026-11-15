package adminui

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
)

const (
	testMAC   = "D8:3B:DA:00:00:01"
	testModel = "xiao_epaper_display"
)

// addState records a wake of the device with the friendly ID, with the image
// and panel size given.
func addState(t *testing.T, tu *testUI, friendly, image string, width, height int64) {
	t.Helper()
	dev, err := device.FindDevice(t.Context(), tu.db.Read, friendly)
	if err != nil {
		t.Fatal(err)
	}
	volts, rssi, wake, source := 3.9, int64(-70), int64(1234), "button"
	err = tu.db.Write.UpsertDeviceState(t.Context(), db.UpsertDeviceStateParams{
		DeviceID: dev.ID, LastSeen: tu.now.Add(-5 * time.Minute).Unix(), BatteryV: &volts, Rssi: &rssi,
		WakeTimeMs: &wake, UpdateSource: &source, LastRefreshRateSent: 900, LastImage: image, Width: &width, Height: &height,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// addBuild adds a firmware build of the model to the catalog.
func addBuild(t *testing.T, tu *testUI, model, version string) {
	t.Helper()
	_, err := tu.db.Write.CreateFirmware(t.Context(), db.CreateFirmwareParams{
		FwModel: model, Version: version, Sha256: fmt.Sprintf("%x", sha256.Sum256([]byte(model+version))),
		Chip: "esp32-s3", Bytes: 1000, Source: "upload", UploadedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// get fetches a page signed in.
func (tu *testUI) get(t *testing.T, c *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	return tu.do(t, "GET", path, "", "Cookie", cookieHeader(c))
}

// post sends a form signed in.
func (tu *testUI) post(t *testing.T, c *http.Cookie, path string, form url.Values, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	return tu.do(t, "POST", path, form.Encode(), append([]string{"Cookie", cookieHeader(c)}, headers...)...)
}

func TestThumbnail(t *testing.T) {
	for _, tc := range []struct {
		name          string
		image         string
		width, height int64
		want          []string
		notWant       []string
	}{
		{
			name: "image with panel size", image: "0123abcd", width: 800, height: 480,
			want: []string{
				`<img src="/images/0123abcd.png" alt="Current screen of Kitchen" width="800" height="480" loading="lazy">`,
			},
			notWant: []string{"No screen yet"},
		},
		{
			name: "image without panel size", image: "0123abcd",
			want:    []string{`<img src="/images/0123abcd.png" alt="Current screen of Kitchen" loading="lazy">`},
			notWant: []string{"No screen yet"},
		},
		{
			name: "no image yet", width: 800, height: 480,
			want:    []string{`<div class="screen screen-empty">No screen yet</div>`},
			notWant: []string{"<img", "/images/"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			addState(t, tu, "ABC123", tc.image, tc.width, tc.height)
			c := tu.signIn(t)
			body := tu.get(t, c, "/devices").Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("list lacks %q:\n%s", want, body)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(body, bad) {
					t.Errorf("list has %q", bad)
				}
			}
			// The page's own image is the one thing that should load at once.
			detail := tu.get(t, c, "/devices/ABC123").Body.String()
			if strings.Contains(detail, "loading=") {
				t.Errorf("detail page has lazy loading:\n%s", detail)
			}
		})
	}
}

func TestCardLinksToDevice(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	addDevice(t, tu, "D8:3B:DA:00:00:02", "XYZ789", "", 0)
	body := tu.get(t, tu.signIn(t), "/devices").Body.String()
	for _, id := range []string{"ABC123", "XYZ789"} {
		if n := strings.Count(body, `href="/devices/`+id+`"`); n != 1 {
			t.Errorf("%d links to %s, want 1", n, id)
		}
	}
	// The card is one link, with no other link or button inside it.
	if n := strings.Count(body, `class="device-link"`); n != 2 {
		t.Errorf("%d card links, want 2", n)
	}
}

func TestDeviceDetail(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	if err := tu.db.Write.UpdateDeviceFirmware(t.Context(), db.UpdateDeviceFirmwareParams{FwModel: testModel, FwVersion: "1.8.19", ID: 1}); err != nil {
		t.Fatal(err)
	}
	addState(t, tu, "ABC123", "0123abcd", 800, 480)
	w := tu.get(t, tu.signIn(t), "/devices/abc123")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	// The time is 1,000,000 s after the epoch, and the zone is UTC+5:30.
	// Last seen is 5 minutes before that and the next wake 15 minutes later.
	for _, want := range []string{
		"<h1>Kitchen</h1>", `<a class="back" href="/devices">`, `class="badge badge-ok">Online`,
		`<img src="/images/0123abcd.png" alt="Current screen of Kitchen" width="800" height="480">`,
		"<dd>ABC123</dd>", "<dd>" + testMAC + "</dd>", "<dd>" + testModel + "</dd>", "<dd>1.8.19</dd>",
		`<time datetime="1970-01-12T13:41:40Z">12 Jan 1970 19:11 TST</time> <span class="rel">(5 min ago)</span>`,
		`<time datetime="1970-01-12T13:56:40Z">12 Jan 1970 19:26 TST</time>`,
		`<time datetime="1970-01-01T00:00:01Z">1 Jan 1970 05:30 TST</time>`,
		"<dd>3.90 V</dd>", "<dd>-70 dBm</dd>", "<dd>1.2 s</dd>", "<dd>button</dd>", "<dd>800 × 480</dd>",
		`<form method="post" action="/devices/ABC123">`, "This device has no playlist items.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"api_key", "ApiKey", "<script", "style="} {
		if strings.Contains(body, bad) {
			t.Errorf("page has %q", bad)
		}
	}
}

func TestDeviceDetailMissingValues(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "", 0)
	body := tu.get(t, tu.signIn(t), "/devices/ABC123").Body.String()
	// Model, firmware, last seen, next wake, battery, signal, wake time,
	// wake cause and panel size.
	if n := strings.Count(body, "<dd>–</dd>"); n != 9 {
		t.Errorf("%d placeholders, want 9:\n%s", n, body)
	}
	for _, want := range []string{"<h1>ABC123</h1>", "No screen yet", "Never seen"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestDeviceNotFound(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	c := tu.signIn(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/devices/ZZZ999"},
		{"POST", "/devices/ZZZ999"},
	} {
		w := tu.do(t, tc.method, tc.path, "", "Cookie", cookieHeader(c))
		body := w.Body.String()
		if w.Code != http.StatusNotFound || !strings.Contains(body, "No such device") || !strings.Contains(body, `<nav class="nav"`) {
			t.Errorf("%s %s: status %d, body %s; want a 404 page in the shell", tc.method, tc.path, w.Code, body)
		}
	}
}

func TestDeviceNeedsSession(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/devices/ABC123", ""},
		{"POST", "/devices/ABC123", "name=Hacked"},
		{"POST", "/join", "action=open"},
	} {
		w := tu.do(t, tc.method, tc.path, tc.body)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
			t.Errorf("%s %s signed out: %d to %q, want 303 to /login", tc.method, tc.path, w.Code, w.Header().Get("Location"))
		}
	}
	dev, _ := device.FindDevice(t.Context(), tu.db.Read, "ABC123")
	if dev.Name != "Kitchen" {
		t.Errorf("a signed-out post renamed the device to %q", dev.Name)
	}
	if until, _ := device.JoinUntil(t.Context(), tu.db.Read); !until.IsZero() {
		t.Error("a signed-out post opened the join window")
	}
}

func TestDeviceSettingsSave(t *testing.T) {
	for _, tc := range []struct {
		name        string
		form        url.Values
		wantName    string
		wantUpdates int64
		wantPin     string
	}{
		{"all fields", url.Values{"name": {"  Hall  "}, "updates_enabled": {"1"}, "fw_pin": {"1.8.17"}}, "Hall", 1, "1.8.17"},
		{"unchecked box turns updates off", url.Values{"name": {"Hall"}}, "Hall", 0, ""},
		{"empty name clears it", url.Values{"updates_enabled": {"1"}}, "", 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			c := tu.signIn(t)
			w := tu.post(t, c, "/devices/ABC123", tc.form)
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/devices/ABC123" {
				t.Fatalf("status %d to %q, want 303 to the device: %s", w.Code, w.Header().Get("Location"), w.Body)
			}
			dev, err := device.FindDevice(t.Context(), tu.db.Read, testMAC)
			if err != nil || dev.Name != tc.wantName || dev.UpdatesEnabled != tc.wantUpdates || dev.FwPin != tc.wantPin {
				t.Errorf("device %+v, %v; want name %q, updates %d, pin %q", dev, err, tc.wantName, tc.wantUpdates, tc.wantPin)
			}
		})
	}
}

func TestDeviceSettingsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name    string
		form    url.Values
		message string
		kept    []string
	}{
		{
			"name too long",
			url.Values{"name": {strings.Repeat("n", 65)}, "updates_enabled": {"1"}},
			"Name is over 64 bytes.",
			[]string{`value="` + strings.Repeat("n", 65) + `"`, " checked"},
		},
		{
			"name with a newline",
			url.Values{"name": {"a\nb"}},
			"Name has a control character.",
			[]string{"value=\"a\nb\""},
		},
		{
			"pin that is not a version",
			url.Values{"name": {"Hall"}, "fw_pin": {"latest"}, "updates_enabled": {"1"}},
			`Version &#34;latest&#34; is not dotted numbers.`,
			[]string{`value="Hall"`, `<option value="latest" selected>`, " checked"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			c := tu.signIn(t)
			w := tu.post(t, c, "/devices/ABC123", tc.form)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", w.Code)
			}
			body := w.Body.String()
			i := strings.Index(body, `<p class="error" role="alert">`+tc.message)
			if j := strings.Index(body, "<form method"); i < 0 || j < i {
				t.Errorf("the message %q is not above the form:\n%s", tc.message, body)
			}
			for _, want := range tc.kept {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks the submitted %q:\n%s", want, body)
				}
			}
			dev, _ := device.FindDevice(t.Context(), tu.db.Read, testMAC)
			if dev.Name != "Kitchen" || dev.UpdatesEnabled != 0 || dev.FwPin != "" {
				t.Errorf("a refused form changed the device: %+v", dev)
			}
		})
	}
}

func TestDeviceSettingsFormTooBig(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	w := tu.post(t, tu.signIn(t), "/devices/ABC123", url.Values{"name": {strings.Repeat("n", maxFormBody)}})
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "The form is over") {
		t.Errorf("status %d, body %s; want 413 and a message", w.Code, w.Body)
	}
}

func TestDeviceSettingsCrossOrigin(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	c := tu.signIn(t)
	for _, path := range []string{"/devices/ABC123", "/join"} {
		w := tu.post(t, c, path, url.Values{"name": {"Hacked"}, "action": {"open"}}, "Sec-Fetch-Site", "cross-site")
		if w.Code != http.StatusForbidden {
			t.Errorf("cross-origin post to %s: status %d, want 403", path, w.Code)
		}
	}
	dev, _ := device.FindDevice(t.Context(), tu.db.Read, testMAC)
	if dev.Name != "Kitchen" {
		t.Errorf("a cross-origin post renamed the device to %q", dev.Name)
	}
	if until, _ := device.JoinUntil(t.Context(), tu.db.Read); !until.IsZero() {
		t.Error("a cross-origin post opened the join window")
	}
}

func TestPinOptions(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	if err := tu.db.Write.UpdateDeviceFirmware(t.Context(), db.UpdateDeviceFirmwareParams{FwModel: testModel, ID: 1}); err != nil {
		t.Fatal(err)
	}
	addBuild(t, tu, testModel, "1.8.9")
	addBuild(t, tu, testModel, "1.8.19")
	addBuild(t, tu, "og", "2.0.1")
	c := tu.signIn(t)
	body := tu.get(t, c, "/devices/ABC123").Body.String()
	// Newest first, and only the device's model.
	want := `<option value="">No pin</option>
<option value="1.8.19">1.8.19</option>
<option value="1.8.9">1.8.9</option>
</select>`
	if !strings.Contains(body, want) {
		t.Errorf("page lacks the pin list %q:\n%s", want, body)
	}
	// A pin that is no longer in the catalog stays selected.
	if w := tu.post(t, c, "/devices/ABC123", url.Values{"fw_pin": {"1.7.0"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d", w.Code)
	}
	body = tu.get(t, c, "/devices/ABC123").Body.String()
	if !strings.Contains(body, `<option value="1.7.0" selected>1.7.0</option>`) {
		t.Errorf("page lacks the selected pin:\n%s", body)
	}
}

func TestDevicePlaylist(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	ctx := t.Context()
	recipe, err := tu.db.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "weather", Source: "x", RefreshInterval: 15, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		duration time.Duration
	}{{"Weather <now>", 90 * time.Minute}, {"News", 0}} {
		inst, err := tu.db.Write.CreateInstance(ctx, db.CreateInstanceParams{RecipeID: recipe.ID, Name: tc.name, FieldValues: "{}", CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := playlist.AddItem(ctx, tu.db, "", 1, inst.ID, tc.duration); err != nil {
			t.Fatal(err)
		}
	}
	body := tu.get(t, tu.signIn(t), "/devices/ABC123").Body.String()
	want := `<li><span class="playlist-name">Weather &lt;now&gt;</span><span class="rel">1 h 30 min</span></li>
<li><span class="playlist-name">News</span><span class="rel">Until a button press</span></li>`
	if !strings.Contains(body, want) {
		t.Errorf("page lacks the playlist %q:\n%s", want, body)
	}
	if strings.Contains(body, "no playlist items") {
		t.Error("page says the playlist is empty")
	}
}

func TestDeviceNameEscaping(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	const name = `<b>"x"</b>`
	addDevice(t, tu, testMAC, "ABC123", name, 0)
	addState(t, tu, "ABC123", "0123abcd", 800, 480)
	c := tu.signIn(t)
	for _, path := range []string{"/devices", "/devices/ABC123"} {
		body := tu.get(t, c, path).Body.String()
		if strings.Contains(body, "<b>") {
			t.Errorf("%s: a device name was not escaped:\n%s", path, body)
		}
		for _, want := range []string{`alt="Current screen of &lt;b&gt;&#34;x&#34;&lt;/b&gt;"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
	}
	body := tu.get(t, c, "/devices/ABC123").Body.String()
	for _, want := range []string{`<h1>&lt;b&gt;&#34;x&#34;&lt;/b&gt;</h1>`, `value="&lt;b&gt;&#34;x&#34;&lt;/b&gt;"`} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page lacks %q:\n%s", want, body)
		}
	}
}

func TestJoinWindow(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	c := tu.signIn(t)
	closed := []string{"New devices cannot register.", `name="action" value="open">Open for 10 minutes</button>`}
	open := []string{
		`New devices can register until <time datetime="1970-01-12T13:56:40Z">19:26 TST</time>.`,
		`name="action" value="close">Close</button>`,
	}
	check := func(label string, shown, hidden []string) {
		t.Helper()
		body := tu.get(t, c, "/devices").Body.String()
		for _, want := range shown {
			if !strings.Contains(body, want) {
				t.Errorf("%s: page lacks %q:\n%s", label, want, body)
			}
		}
		for _, bad := range hidden {
			if strings.Contains(body, bad) {
				t.Errorf("%s: page has %q", label, bad)
			}
		}
		if !strings.Contains(body, `<form class="notice" method="post" action="/join">`) {
			t.Errorf("%s: page lacks the join form", label)
		}
	}
	check("never opened", closed, open)

	post := func(action string) *httptest.ResponseRecorder {
		return tu.post(t, c, "/join", url.Values{"action": {action}})
	}
	if w := post("open"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/devices" {
		t.Fatalf("open: %d to %q, want 303 to /devices", w.Code, w.Header().Get("Location"))
	}
	until, err := device.JoinUntil(t.Context(), tu.db.Read)
	if err != nil || !until.Equal(tu.now.Add(10*time.Minute)) {
		t.Errorf("window closes at %v, %v; want in 10 minutes", until, err)
	}
	check("open", open, closed)

	tu.now = tu.now.Add(11 * time.Minute)
	check("expired", closed, open)
	tu.now = tu.now.Add(-11 * time.Minute)

	if w := post("close"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/devices" {
		t.Fatalf("close: %d to %q, want 303 to /devices", w.Code, w.Header().Get("Location"))
	}
	check("closed", closed, open)
}

func TestJoinWindowBadAction(t *testing.T) {
	for _, action := range []string{"", "toggle", "OPEN", "open "} {
		tu := newTestUI(t, Config{}, true)
		w := tu.post(t, tu.signIn(t), "/join", url.Values{"action": {action}})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Choose Open or Close.") {
			t.Errorf("action %q: status %d, body %s; want 400 and a message", action, w.Code, w.Body)
		}
		if until, _ := device.JoinUntil(t.Context(), tu.db.Read); !until.IsZero() {
			t.Errorf("action %q opened the join window", action)
		}
	}
}

func TestListTimesUseZone(t *testing.T) {
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", tu.now.Add(-time.Hour).Unix())
	body := tu.get(t, tu.signIn(t), "/devices").Body.String()
	want := `<time datetime="1970-01-12T12:46:40Z" title="12 Jan 1970 18:16 TST">1 h ago</time>`
	if !strings.Contains(body, want) {
		t.Errorf("list lacks %q:\n%s", want, body)
	}
}

// TestUntilText checks that the join window's closing time leaves out the
// date only when it is today in the server's zone.
func TestUntilText(t *testing.T) {
	s := &Server{zone: testZone}
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, testZone)
	for _, tc := range []struct {
		name  string
		until time.Time
		want  string
	}{
		{"later today", now.Add(10 * time.Minute), "12:10 TST"},
		{"last minute of today", time.Date(2026, 3, 1, 23, 59, 0, 0, testZone), "23:59 TST"},
		{"tomorrow", time.Date(2026, 3, 2, 0, 0, 0, 0, testZone), "2 Mar 2026 00:00 TST"},
		{"same day next year", now.AddDate(1, 0, 0), "1 Mar 2027 12:00 TST"},
		{"today in the zone, tomorrow in UTC", time.Date(2026, 3, 1, 1, 0, 0, 0, testZone), "01:00 TST"},
	} {
		if got := s.untilText(tc.until, now.UTC()); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
