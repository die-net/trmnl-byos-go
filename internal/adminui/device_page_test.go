package adminui

import (
	"crypto/sha256"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		`<form method="post" action="/devices/ABC123#settings">`, "This device has no playlist items.",
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/devices/ABC123", ""},
		{"POST", "/devices/ABC123", "name=Hacked"},
		{"POST", "/join", "action=open"},
		{"POST", "/devices/ABC123/playlists", "playlist=1&order=1&to=0"},
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

// TestDevicePlaylistMoveTooBig checks that the move form's body is bounded
// like every other form's.
func TestDevicePlaylistMoveTooBig(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	big := url.Values{"playlist": {strings.Repeat("1", maxFormBody)}}
	if w := tu.post(t, tu.signIn(t), "/devices/ABC123/playlists", big); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", w.Code)
	}
}

// withState returns the form with the settings state and the row ID a page
// of the device with testMAC would post now.
func withState(t *testing.T, tu *testUI, form url.Values) url.Values {
	t.Helper()
	dev, err := device.FindDevice(t.Context(), tu.db.Read, testMAC)
	if err != nil {
		t.Fatal(err)
	}
	form.Set("settings_state", device.SettingsState(&dev))
	form.Set("seen_device", strconv.FormatInt(dev.ID, 10))
	return form
}

func TestDeviceSettingsSave(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			c := tu.signIn(t)
			w := tu.post(t, c, "/devices/ABC123", withState(t, tu, tc.form))
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
	t.Parallel()
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
			`The version &#34;latest&#34; is not dotted numbers, such as 1.8.19.`,
			[]string{`value="Hall"`, `<option value="latest" selected>`, " checked"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			c := tu.signIn(t)
			w := tu.post(t, c, "/devices/ABC123", withState(t, tu, tc.form))
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
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	w := tu.post(t, tu.signIn(t), "/devices/ABC123", url.Values{"name": {strings.Repeat("n", maxFormBody)}})
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "The form is over") {
		t.Errorf("status %d, body %s; want 413 and a message", w.Code, w.Body)
	}
}

func TestDeviceSettingsCrossOrigin(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	c := tu.signIn(t)
	for _, path := range []string{"/devices/ABC123", "/join", "/devices/ABC123/playlists"} {
		w := tu.post(t, c, path, url.Values{"name": {"Hacked"}, "action": {"open"}, "playlist": {"1"}, "index": {"0"}}, "Sec-Fetch-Site", "cross-site")
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
	t.Parallel()
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
	if w := tu.post(t, c, "/devices/ABC123", withState(t, tu, url.Values{"fw_pin": {"1.7.0"}})); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d", w.Code)
	}
	body = tu.get(t, c, "/devices/ABC123").Body.String()
	if !strings.Contains(body, `<option value="1.7.0" selected>1.7.0</option>`) {
		t.Errorf("page lacks the selected pin:\n%s", body)
	}
}

func TestDevicePlaylist(t *testing.T) {
	t.Parallel()
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
		if _, err := addItemTo(ctx, tu.db, "", "ABC123", inst.ID, tc.duration); err != nil {
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

// TestDevicePlaylistZone checks that the device page reads item schedules in
// the server's zone. The test clock reads 19:16 there and 13:46 in UTC.
func TestDevicePlaylistZone(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, tc := range []struct {
		name       string
		start, end int64
		shown      bool
	}{
		{"open in the server's zone", 19 * 60, 20 * 60, true},
		{"open in UTC only", 13 * 60, 14 * 60, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			if got := tu.now.In(testZone).Format("15:04"); got != "19:16" {
				t.Fatalf("the test clock reads %s in the server's zone, want 19:16", got)
			}
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			recipe, err := tu.db.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "weather", Source: "x", RefreshInterval: 15, InstalledAt: 1})
			if err != nil {
				t.Fatal(err)
			}
			var item db.PlaylistItem
			for _, name := range []string{"Weather", "News"} {
				inst, err := tu.db.Write.CreateInstance(ctx, db.CreateInstanceParams{RecipeID: recipe.ID, Name: name, FieldValues: "{}", CreatedAt: 1})
				if err != nil {
					t.Fatal(err)
				}
				if item, err = addItemTo(ctx, tu.db, "", "ABC123", inst.ID, 0); err != nil {
					t.Fatal(err)
				}
			}
			s := playlist.Always
			s.WindowStart, s.WindowEnd = tc.start, tc.end
			edit := s.Full()
			if err := playlist.UpdateItem(ctx, tu.db, item.ID, playlist.ItemUpdate{Schedule: &edit}); err != nil {
				t.Fatal(err)
			}
			body := tu.get(t, tu.signIn(t), "/devices/ABC123").Body.String()
			if got := strings.Contains(body, `<span class="playlist-name">News</span>`); got != tc.shown {
				t.Errorf("News listed = %v, want %v:\n%s", got, tc.shown, body)
			}
		})
	}
}

func TestDeviceNameEscaping(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// playlistMove is the fields of a form that moves a playlist on a device
// page: the playlist, the playlists the page shows in their order, and the
// place it moves to.
func playlistMove(pid int64, order string, to int64) string {
	return `<input type="hidden" name="playlist" value="` + id(pid) + `"><input type="hidden" name="order" value="` + order + `"><input type="hidden" name="to" value="` + id(to) + `">`
}

func TestDevicePlaylistsPanel(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	b, a, c := newPlaylist(t, tu, "Weekdays <b>"), newPlaylist(t, tu, "Always"), newPlaylist(t, tu, "Late")
	linkTo(t, tu, "ABC123", b, a, c)
	days, err := playlist.DaysFromNames([]string{"mon", "tue", "wed", "thu", "fri"})
	if err != nil {
		t.Fatal(err)
	}
	sched := playlist.Schedule{Days: days, WindowStart: 6 * 60, WindowEnd: 9 * 60}.Full()
	if err := playlist.UpdatePlaylist(t.Context(), tu.db, playlist.ByID(b), playlist.Update{Schedule: &sched}); err != nil {
		t.Fatal(err)
	}
	body := tu.get(t, tu.signIn(t), "/devices/ABC123").Body.String()
	shown := id(b) + "," + id(a) + "," + id(c)
	form := func(pid, to int64, label string, disabled bool) string {
		d := ""
		if disabled {
			d = " disabled"
		}
		return `<form method="post" action="/devices/ABC123/playlists#playlists">` + playlistMove(pid, shown, to) +
			`<button class="btn btn-secondary btn-icon" type="submit" aria-label="Move ` + label + `"` + d + `>`
	}
	wantInOrder(t, body, `<section class="panel" id="playlists"`, `<h2 id="playlists-heading">Playlists</h2>`,
		"The device shows the first of these that is scheduled now.", `<ol class="items">`,
		`<a class="item-name" href="/playlists#playlist-`+id(b)+`">Weekdays &lt;b&gt;</a>`, `Weekdays, 06:00 to 09:00`,
		form(b, -1, "Weekdays &lt;b&gt; earlier", true), form(b, 1, "Weekdays &lt;b&gt; later", false),
		`<a class="item-name" href="/playlists#playlist-`+id(a)+`">Always</a>`, `Every day, all day`,
		form(a, 0, "Always earlier", false), form(a, 2, "Always later", false),
		`<a class="item-name" href="/playlists#playlist-`+id(c)+`">Late</a>`,
		form(c, 1, "Late earlier", false), form(c, 3, "Late later", true))
	if strings.Contains(body, "<b>") {
		t.Error("page has an unescaped playlist name")
	}
	if n := strings.Count(body, " disabled>"); n != 2 {
		t.Errorf("%d disabled buttons, want the first one's up and the last one's down", n)
	}
}

func TestDevicePlaylistsPanelFew(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lists []string
		want  []string
		moves bool
	}{
		{"none", nil, []string{"This device is linked to no playlist. Link one from a playlist's page.", `<a href="/playlists">All playlists</a>`}, false},
		{"one", []string{"Solo"}, []string{`<a class="item-name" href="/playlists#playlist-1">Solo</a>`, "The device shows the first of these that is scheduled now."}, false},
		{"two", []string{"One", "Two"}, []string{"Move One later", "Move Two earlier"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			for _, name := range tc.lists {
				linkTo(t, tu, "ABC123", newPlaylist(t, tu, name))
			}
			body := tu.get(t, tu.signIn(t), "/devices/ABC123").Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
			if got := strings.Contains(body, `action="/devices/ABC123/playlists#playlists"`); got != tc.moves {
				t.Errorf("move forms shown = %v, want %v", got, tc.moves)
			}
			if len(tc.lists) == 0 && strings.Contains(body, "The device shows the first") {
				t.Error("the page explains the order of no playlists")
			}
		})
	}
}

func TestDeviceMovePlaylist(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		move string // the playlist to move.
		to   string
		want string // the order after.
	}{
		{"up", "c", "1", "a,c,b"},
		{"to the front", "c", "0", "c,a,b"},
		{"down", "a", "1", "b,a,c"},
		{"to the end", "a", "2", "b,c,a"},
		{"stays", "b", "1", "a,b,c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "", 0)
			ids := map[string]int64{}
			order := make([]int64, 0, 3)
			for _, n := range []string{"a", "b", "c"} {
				ids[n] = newPlaylist(t, tu, n)
				order = append(order, ids[n])
			}
			linkTo(t, tu, "ABC123", order...)
			w := tu.post(t, tu.signIn(t), "/devices/ABC123/playlists", url.Values{"playlist": {id(ids[tc.move])}, "order": {joinIDs(order)}, "to": {tc.to}})
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/devices/ABC123#playlists" {
				t.Fatalf("status %d to %q: %s", w.Code, w.Header().Get("Location"), w.Body)
			}
			var got []string
			for _, pid := range linkedIDs(t, tu, "ABC123") {
				for n, v := range ids {
					if v == pid {
						got = append(got, n)
					}
				}
			}
			if strings.Join(got, ",") != tc.want {
				t.Errorf("order %v, want %s", got, tc.want)
			}
		})
	}
}

func TestDeviceMovePlaylistRefused(t *testing.T) {
	t.Parallel()
	move := func(list, order, to string) url.Values {
		return url.Values{"playlist": {list}, "order": {order}, "to": {to}}
	}
	const path = "/devices/ABC123/playlists"
	for _, tc := range []struct {
		name   string
		path   string
		form   func(a, b, stranger int64) url.Values
		status int
		want   string
	}{
		// b was moved or a playlist unlinked or linked after the page was
		// shown, so the order the form saw is not the order the device has.
		{"stale order", path, func(a, b, _ int64) url.Values { return move(id(a), id(b)+","+id(a), "0") }, http.StatusConflict, orderChanged},
		{"stale order, same target", path, func(a, b, _ int64) url.Values { return move(id(b), id(b)+","+id(a), "0") }, http.StatusConflict, orderChanged},
		{"stale order, a link since added", path, func(a, _, _ int64) url.Values { return move(id(a), id(a), "0") }, http.StatusConflict, orderChanged},
		{"stale order, a link since removed", path, func(a, b, s int64) url.Values { return move(id(a), id(a)+","+id(b)+","+id(s), "1") }, http.StatusConflict, orderChanged},
		{"stale order, past the end", path, func(a, b, _ int64) url.Values { return move(id(a), id(b)+","+id(a), "5") }, http.StatusConflict, orderChanged},
		{"no order shown", path, func(a, _, _ int64) url.Values { return move(id(a), "", "1") }, http.StatusConflict, orderChanged},
		{"place past the end", path, func(a, b, _ int64) url.Values { return move(id(a), id(a)+","+id(b), "5") }, http.StatusBadRequest, orderChanged},
		{"negative place", path, func(a, b, _ int64) url.Values { return move(id(a), id(a)+","+id(b), "-1") }, http.StatusBadRequest, orderChanged},
		{"no playlist", path, func(a, b, _ int64) url.Values { return url.Values{"order": {id(a) + "," + id(b)}, "to": {"0"}} }, http.StatusBadRequest, "Choose a playlist to move."},
		{"playlist not a number", path, func(a, b, _ int64) url.Values { return move("x", id(a)+","+id(b), "0") }, http.StatusBadRequest, "Choose a playlist to move."},
		{"no order", path, func(a, _, _ int64) url.Values { return url.Values{"playlist": {id(a)}, "to": {"1"}} }, http.StatusBadRequest, "Choose a playlist to move."},
		{"order not numbers", path, func(a, _, _ int64) url.Values { return move(id(a), id(a)+",x", "1") }, http.StatusBadRequest, "Choose a playlist to move."},
		{"order with a gap", path, func(a, _, _ int64) url.Values { return move(id(a), id(a)+",,", "1") }, http.StatusBadRequest, "Choose a playlist to move."},
		{"no to", path, func(a, b, _ int64) url.Values { return url.Values{"playlist": {id(a)}, "order": {id(a) + "," + id(b)}} }, http.StatusBadRequest, "Choose a playlist to move."},
		{"to not a number", path, func(a, b, _ int64) url.Values { return move(id(a), id(a)+","+id(b), "first") }, http.StatusBadRequest, "Choose a playlist to move."},
		{"not linked", path, func(a, b, s int64) url.Values { return move(id(s), id(a)+","+id(b), "0") }, http.StatusConflict, "That playlist is no longer linked to this device."},
		{"unknown playlist", path, func(a, b, _ int64) url.Values { return move("999", id(a)+","+id(b), "0") }, http.StatusConflict, "That playlist no longer exists."},
		{"unknown device", "/devices/ZZZ999/playlists", func(a, b, _ int64) url.Values { return move(id(a), id(a)+","+id(b), "0") }, http.StatusNotFound, "No such device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			a, b, stranger := newPlaylist(t, tu, "a"), newPlaylist(t, tu, "b"), newPlaylist(t, tu, "stranger")
			linkTo(t, tu, "ABC123", a, b)
			w := tu.post(t, tu.signIn(t), tc.path, tc.form(a, b, stranger))
			body := w.Body.String()
			want := template.HTMLEscapeString(tc.want)
			if w.Code != tc.status || !strings.Contains(body, want) {
				t.Fatalf("status %d, body %s; want %d with %q", w.Code, body, tc.status, want)
			}
			if tc.status != http.StatusNotFound {
				// The device page, with the reason in the playlists panel
				// above the order as it is now.
				wantInOrder(t, body, "<h1>Kitchen</h1>", `id="playlists"`, `<p class="error" role="alert">`+want+`</p>`, `<ol class="items">`)
			}
			// Nothing moved, and a playlist that was not linked was not linked by the move.
			if got := linkedIDs(t, tu, "ABC123"); len(got) != 2 || got[0] != a || got[1] != b {
				t.Errorf("links %v after a refused move, want %d and %d", got, a, b)
			}
		})
	}
}

// TestDeviceMoveStalePage checks the case the form's order field is for. The
// page showed a, b, c, and another change swapped a and b. A move of c from
// the page is refused, since it would put c where the admin did not mean, and
// the order is left as the other change made it.
func TestDeviceMoveStalePage(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "", 0)
	a, b, c := newPlaylist(t, tu, "a"), newPlaylist(t, tu, "b"), newPlaylist(t, tu, "c")
	linkTo(t, tu, "ABC123", a, b, c)
	session := tu.signIn(t)
	shown := joinIDs([]int64{a, b, c})
	// The other change: a moves after b.
	swap := url.Values{"playlist": {id(a)}, "order": {shown}, "to": {"1"}}
	if w := tu.post(t, session, "/devices/ABC123/playlists", swap); w.Code != http.StatusSeeOther {
		t.Fatalf("swap: status %d: %s", w.Code, w.Body)
	}
	stale := url.Values{"playlist": {id(c)}, "order": {shown}, "to": {"1"}}
	w := tu.post(t, session, "/devices/ABC123/playlists", stale)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), template.HTMLEscapeString(orderChanged)) {
		t.Fatalf("stale move: status %d, body %s; want 409 saying the order changed", w.Code, w.Body)
	}
	if got := linkedIDs(t, tu, "ABC123"); len(got) != 3 || got[0] != b || got[1] != a || got[2] != c {
		t.Errorf("links %v, want %d, %d, %d", got, b, a, c)
	}
	// The same move from the order the refusal shows goes through.
	fresh := url.Values{"playlist": {id(c)}, "order": {joinIDs([]int64{b, a, c})}, "to": {"1"}}
	if w := tu.post(t, session, "/devices/ABC123/playlists", fresh); w.Code != http.StatusSeeOther {
		t.Fatalf("move from the current order: status %d: %s", w.Code, w.Body)
	}
	if got := linkedIDs(t, tu, "ABC123"); len(got) != 3 || got[0] != b || got[1] != c || got[2] != a {
		t.Errorf("links %v, want %d, %d, %d", got, b, c, a)
	}
}

// TestDeviceMoveDoesNotLink checks that a move for a playlist that exists
// but is not linked does not link it, even to the end.
func TestDeviceMoveDoesNotLink(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "", 0)
	stranger := newPlaylist(t, tu, "stranger")
	for _, to := range []string{"0", "1", "5"} {
		w := tu.post(t, tu.signIn(t), "/devices/ABC123/playlists", url.Values{"playlist": {id(stranger)}, "order": {""}, "to": {to}})
		if w.Code != http.StatusConflict {
			t.Errorf("to %s: status %d, want 409", to, w.Code)
		}
	}
	if got := linkedIDs(t, tu, "ABC123"); len(got) != 0 {
		t.Errorf("links %v, want none", got)
	}
}
