package deviceapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
)

const testMAC = "D8:3B:DA:12:34:56"

// Header sets as the firmware's buildSetupHeaders, buildDisplayHeaders and
// buildLogHeaders send them for a TRMNL OG DIY kit.
func setupHeaders(mac string) map[string]string {
	return map[string]string{
		"ID":           mac,
		"Content-Type": "application/json",
		"FW-Version":   "1.7.8",
		"Model":        "xiao_epaper_display",
		"Width":        "800",
		"Height":       "480",
		"RSSI":         "-61",
	}
}

func displayHeaders(mac, token string) map[string]string {
	return map[string]string{
		"ID":                  mac,
		"Content-Type":        "application/json",
		"Update-Source":       "timer",
		"Access-Token":        token,
		"Refresh-Rate":        "900",
		"Battery-Voltage":     "4.07",
		"FW-Version":          "1.7.8",
		"FW-Commit":           "5f8a43d",
		"Model":               "xiao_epaper_display",
		"Image-Cached":        "false",
		"Wake-Time":           "3120",
		"Reset-Reason":        "ESP_RST_DEEPSLEEP",
		"RSSI":                "-61",
		"Temperature-Profile": "true",
		"Width":               "800",
		"Height":              "480",
		"Log-Level":           "info",
	}
}

type testServer struct {
	*httptest.Server
	srv *Server
	db  *db.Client
	now time.Time
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	dir := t.TempDir()
	client, err := db.Open(t.Context(), filepath.Join(dir, "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	images, err := imagestore.New(filepath.Join(dir, "images"))
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{db: client, now: time.Unix(1_800_000_000, 0)}
	ts.srv = New(client, images, Config{RefreshRate: 15 * time.Minute})
	ts.srv.now = func() time.Time { return ts.now }
	mux := http.NewServeMux()
	ts.srv.Register(mux)
	ts.Server = httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// do makes a request and returns the response's status and body.
func (ts *testServer) do(t *testing.T, method, path string, headers map[string]string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, data
}

// getJSON makes a request and decodes the JSON response into a map, so a
// test sees each field's JSON type as the firmware would.
func (ts *testServer) getJSON(t *testing.T, path string, headers map[string]string) map[string]any {
	t.Helper()
	code, data := ts.do(t, http.MethodGet, path, headers, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s = HTTP %d, want 200", path, code)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return m
}

func (ts *testServer) openJoinWindow(t *testing.T) {
	t.Helper()
	if err := SetJoinWindow(t.Context(), ts.db, ts.now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

// register opens the join window, sets up testMAC, and returns its token.
func (ts *testServer) register(t *testing.T) string {
	t.Helper()
	ts.openJoinWindow(t)
	m := ts.getJSON(t, "/api/setup", setupHeaders(testMAC))
	key, _ := m["api_key"].(string)
	if m["status"] != float64(statusSetupOK) || key == "" {
		t.Fatalf("setup = %v", m)
	}
	return key
}

// fetchImage gets an image_url and checks it is a PNG whose name is the
// response's filename.
func (ts *testServer) fetchImage(t *testing.T, url, filename string) {
	t.Helper()
	if !strings.HasPrefix(url, ts.URL+"/images/") {
		t.Fatalf("image_url %q is not an absolute URL on this server", url)
	}
	code, data := ts.do(t, http.MethodGet, strings.TrimPrefix(url, ts.URL), nil, nil)
	if code != http.StatusOK || !bytes.HasPrefix(data, []byte("\x89PNG")) {
		t.Fatalf("GET %s = %d, %d bytes", url, code, len(data))
	}
	if filename != "" && imagestore.Name(data) != filename {
		t.Fatalf("filename %q is not the image's name %q", filename, imagestore.Name(data))
	}
	if len(filename) > 31 {
		t.Fatalf("filename %q is longer than the firmware keeps", filename)
	}
}

// TestSetupJoinWindow checks that setup works only while the join window
// is open, for a new MAC and for a known one alike, and that each setup
// issues a new key and retires the old one.
func TestSetupJoinWindow(t *testing.T) {
	ts := newTestServer(t)

	m := ts.getJSON(t, "/api/setup", setupHeaders(testMAC))
	if m["status"] != float64(statusSetupUnknown) || m["api_key"] != nil {
		t.Fatalf("closed window: %v", m)
	}
	ts.fetchImage(t, m["image_url"].(string), "")

	ts.openJoinWindow(t)
	ts.now = ts.now.Add(5 * time.Minute)
	m = ts.getJSON(t, "/api/setup", setupHeaders(strings.ToLower(testMAC)))
	oldKey, id := m["api_key"].(string), m["friendly_id"]
	if m["status"] != float64(statusSetupOK) || oldKey == "" || len(id.(string)) != 6 || m["image_url"] != "" {
		t.Fatalf("open window: %v", m)
	}

	// A device that lost its credentials runs setup again.
	ts.now = ts.now.Add(4 * time.Minute)
	m = ts.getJSON(t, "/api/setup", setupHeaders(testMAC))
	newKey := m["api_key"].(string)
	if m["status"] != float64(statusSetupOK) || newKey == "" || newKey == oldKey || m["friendly_id"] != id {
		t.Fatalf("re-setup gave %v, want a new key and the same id", m)
	}
	if m := ts.getJSON(t, "/api/display", displayHeaders(testMAC, oldKey)); m["status"] != float64(statusOK) || m["reset_firmware"] != true {
		t.Fatalf("old key after re-setup: %v, want a reset", m)
	}
	if m := ts.getJSON(t, "/api/display", displayHeaders(testMAC, newKey)); m["status"] != float64(statusOK) {
		t.Fatalf("new key after re-setup: %v", m)
	}

	// After the window closes, knowing a MAC is not enough to get a key.
	ts.now = ts.now.Add(2 * time.Minute)
	for _, mac := range []string{testMAC, "D8:3B:DA:00:00:01"} {
		if m := ts.getJSON(t, "/api/setup", setupHeaders(mac)); m["status"] != float64(statusSetupUnknown) || m["api_key"] != nil {
			t.Fatalf("%s after the window closed: %v", mac, m)
		}
	}
	if m := ts.getJSON(t, "/api/display", displayHeaders(testMAC, newKey)); m["status"] != float64(statusOK) {
		t.Fatalf("key after a refused setup: %v", m)
	}

	devices, err := ts.db.Read.ListDevices(t.Context())
	if err != nil || len(devices) != 1 || devices[0].Mac != testMAC || devices[0].FwModel != "xiao_epaper_display" {
		t.Fatalf("devices = %+v, %v", devices, err)
	}
	if !bytes.Equal(devices[0].ApiKeyHash, hashKey(newKey)) {
		t.Fatalf("stored %x, want the hash of the new key", devices[0].ApiKeyHash)
	}
}

func TestSetupBadID(t *testing.T) {
	ts := newTestServer(t)
	for _, id := range []string{"", "not a mac", "D8:3B:DA:12:34"} {
		if code, _ := ts.do(t, http.MethodGet, "/api/setup", setupHeaders(id), nil); code != http.StatusBadRequest {
			t.Errorf("ID %q: HTTP %d, want 400", id, code)
		}
	}
}

// TestDisplay checks a registered device's response: status 0, a numeric
// refresh_rate, a filename that names the image, the same filename on the
// next wake, and the device's state saved.
func TestDisplay(t *testing.T) {
	ts := newTestServer(t)
	key := ts.register(t)

	m := ts.getJSON(t, "/api/display", displayHeaders(testMAC, key))
	if m["status"] != float64(statusOK) {
		t.Fatalf("status %v, want 0", m["status"])
	}
	if rate, ok := m["refresh_rate"].(float64); !ok || rate != 900 {
		t.Fatalf("refresh_rate %#v, want the number 900", m["refresh_rate"])
	}
	if m["reset_firmware"] != false || m["update_firmware"] != false {
		t.Fatalf("firmware flags: %v", m)
	}
	filename := m["filename"].(string)
	ts.fetchImage(t, m["image_url"].(string), filename)

	ts.now = ts.now.Add(15 * time.Minute)
	m = ts.getJSON(t, "/api/display", displayHeaders(testMAC, key))
	if m["filename"] != filename {
		t.Fatalf("second wake filename %v, want %s", m["filename"], filename)
	}

	refs, err := ts.db.Read.ListReferencedImages(t.Context())
	if err != nil || len(refs) != 1 || refs[0] != filename {
		t.Fatalf("referenced images = %v, %v; want [%s]", refs, err, filename)
	}

	rows, err := ts.db.Read.ListDeviceStatus(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("status rows = %v, %v", rows, err)
	}
	r := rows[0]
	if *r.LastSeen != ts.now.Unix() || *r.BatteryV != 4.07 || *r.Rssi != -61 || *r.WakeTimeMs != 3120 ||
		*r.UpdateSource != "timer" || *r.LastRefreshRateSent != 900 {
		t.Fatalf("state = %+v", r)
	}
}

// TestDisplayFirmwareChange checks that a new Model or FW-Version header
// updates the device.
func TestDisplayFirmwareChange(t *testing.T) {
	ts := newTestServer(t)
	key := ts.register(t)
	h := displayHeaders(testMAC, key)
	h["FW-Version"] = "1.8.0"
	ts.getJSON(t, "/api/display", h)
	dev, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil || dev.FwVersion != "1.8.0" || dev.FwModel != "xiao_epaper_display" {
		t.Fatalf("device = %+v, %v", dev, err)
	}
}

// TestDisplayRefused checks which refused requests get status 202, which
// keeps a device's credentials and Wi-Fi settings, and which get
// reset_firmware, which wipes them: only a registered MAC with a wrong key.
// An unknown MAC, or a request with no ID, may come from a server that lost
// its database, so it is never reset.
func TestDisplayRefused(t *testing.T) {
	ts := newTestServer(t)
	key := ts.register(t)
	for _, tc := range []struct {
		name       string
		mac, token string
		reset      bool
	}{
		{"wrong token", testMAC, key + "x", true},
		{"empty token", testMAC, "", false},
		{"unknown MAC, unknown token", "D8:3B:DA:00:00:01", "nope", false},
		{"no ID, unknown token", "", "nope", false},
		{"another MAC with this key", "D8:3B:DA:00:00:01", key, false},
		{"bad ID", "not a mac", key, false},
	} {
		m := ts.getJSON(t, "/api/display", displayHeaders(tc.mac, tc.token))
		if m["image_url"] != nil || m["reset_firmware"] != tc.reset {
			t.Errorf("%s: %v, want reset_firmware %v", tc.name, m, tc.reset)
			continue
		}
		want := float64(statusNotRegistered)
		if tc.reset {
			want = statusOK
		}
		if m["status"] != want {
			t.Errorf("%s: status %v, want %v", tc.name, m["status"], want)
		}
	}
}

// TestDisplayWithoutID checks that a client that sends no ID header, as
// some non-TRMNL clients do, is found by its token alone.
func TestDisplayWithoutID(t *testing.T) {
	ts := newTestServer(t)
	key := ts.register(t)
	h := displayHeaders(testMAC, key)
	delete(h, "ID")
	if m := ts.getJSON(t, "/api/display", h); m["status"] != float64(statusOK) {
		t.Fatalf("no ID: %v", m)
	}
}

// logBody is a body as the firmware's serializeApiLogRequest and
// serialize_log build it.
func logBody(n int) []byte {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"created_at":1800000000,"id":%d,"message":"entry %d","source_line":42,`+
			`"source_path":"src/bl.cpp","wifi_signal":-61,"wifi_status":"connected","refresh_rate":900,`+
			`"sleep_duration":900,"firmware_version":"1.7.8","special_function":"none","battery_voltage":4.07,`+
			`"wake_reason":"timer","free_heap_size":180000,"max_alloc_size":110000,"level":"info"}`, i, i)
	}
	return []byte(`{"logs":[` + strings.Join(entries, ",") + `]}`)
}

// TestLog checks that log entries are stored whole, that only the newest
// keepLogs are kept, and that a wrong token is refused.
func TestLog(t *testing.T) {
	ts := newTestServer(t)
	key := ts.register(t)
	h := map[string]string{"ID": testMAC, "Access-Token": key, "Content-Type": "application/json", "Accept": "application/json, */*"}

	if code, _ := ts.do(t, http.MethodPost, "/api/log", h, logBody(keepLogs+5)); code != http.StatusNoContent {
		t.Fatalf("POST /api/log = %d", code)
	}
	dev, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := ts.db.Read.ListDeviceLog(t.Context(), db.ListDeviceLogParams{DeviceID: dev.ID, Limit: 2 * keepLogs})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != keepLogs {
		t.Fatalf("%d entries kept, want %d", len(logs), keepLogs)
	}
	newest := logs[0]
	want := fmt.Sprintf("entry %d", keepLogs+4)
	if newest.Message != want || newest.Level != "info" || newest.DeviceAt != 1_800_000_000 ||
		!strings.Contains(newest.Detail, `"wake_reason":"timer"`) {
		t.Fatalf("newest entry = %+v", newest)
	}
	if oldest := logs[len(logs)-1]; oldest.Message != "entry 5" {
		t.Fatalf("oldest kept entry = %q, want entry 5", oldest.Message)
	}

	h["Access-Token"] = "wrong"
	if code, _ := ts.do(t, http.MethodPost, "/api/log", h, logBody(1)); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: HTTP %d, want 401", code)
	}
}
