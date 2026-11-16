package deviceapi

import (
	"bytes"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
)

// strangerMAC is a MAC that never registers.
const strangerMAC = "D8:3B:DA:00:00:01"

// standing reads what the server knows of mac.
func (ts *testServer) standing(t *testing.T, mac string) db.GetMACStandingRow {
	t.Helper()
	s, err := ts.db.Read.GetMACStanding(t.Context(), mac)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// savedState is what the server has saved from devices' requests: each
// device's last wake, image and cursor.
func (ts *testServer) savedState(t *testing.T) string {
	t.Helper()
	var s string
	err := ts.db.Reader.QueryRowContext(t.Context(), `SELECT COALESCE(GROUP_CONCAT(row, ';'), '') FROM (
		SELECT device_state.device_id || ' ' || device_state.last_seen || ' ' || device_state.last_image || ' ' || COALESCE(device_cursor.item_id, '') AS row
		FROM device_state LEFT JOIN device_cursor ON device_cursor.device_id = device_state.device_id
		ORDER BY device_state.device_id)`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// wantReset checks that m tells the device to reset, with status 0 and no
// image.
func wantReset(t *testing.T, what string, m map[string]any) {
	t.Helper()
	if m["status"] != float64(statusOK) || m["reset_firmware"] != true || m["image_url"] != nil {
		t.Errorf("%s: %v, want a reset", what, m)
	}
}

// wantBlocked checks that m is the blocked screen: status 0, an image named
// by its hash, a day's sleep, and no reset or firmware. It returns the
// image's name.
func (ts *testServer) wantBlocked(t *testing.T, what string, m map[string]any) string {
	t.Helper()
	url, _ := m["image_url"].(string)
	name, _ := m["filename"].(string)
	if m["status"] != float64(statusOK) || url == "" || name == "" || m["refresh_rate"] != float64(blockedRefresh) ||
		m["reset_firmware"] != false || m["update_firmware"] != false {
		t.Errorf("%s: %v, want the blocked screen", what, m)
		return ""
	}
	ts.fetchImage(t, url, name)
	return name
}

// TestDeleteResetsDevice checks that a deleted device's next request, and
// every one after until it runs setup again, gets reset_firmware, that its
// tombstone goes when its setup completes, and that an unknown MAC and a
// request with no ID are still never reset.
func TestDeleteResetsDevice(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	old, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := device.Delete(t.Context(), ts.db, testMAC, nil, ts.now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		wantReset(t, "deleted device", ts.getJSON(t, "/api/display", displayHeaders(testMAC, key)))
	}
	noID := displayHeaders(testMAC, key)
	delete(noID, "ID")
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"unknown MAC", displayHeaders(strangerMAC, key)},
		{"no ID", noID},
		{"deleted MAC with no token", displayHeaders(testMAC, "")},
	} {
		if m := ts.getJSON(t, "/api/display", tc.headers); m["status"] != float64(statusNotRegistered) || m["reset_firmware"] == true {
			t.Errorf("%s: %v, want 202 and no reset", tc.name, m)
		}
	}

	// After the reset the device runs setup. With the window closed it is
	// not registered, and the tombstone stays.
	ts.now = ts.now.Add(time.Hour)
	if m := ts.getJSON(t, "/api/setup", setupHeaders(testMAC)); m["status"] != float64(statusSetupUnknown) || m["api_key"] != nil {
		t.Fatalf("setup with the window closed: %v", m)
	}
	if s := ts.standing(t, testMAC); s != (db.GetMACStandingRow{Deleted: 1}) {
		t.Fatalf("standing after a refused setup = %+v", s)
	}
	ts.openJoinWindow(t)
	m := ts.getJSON(t, "/api/setup", setupHeaders(testMAC))
	newKey, _ := m["api_key"].(string)
	if m["status"] != float64(statusSetupOK) || newKey == "" {
		t.Fatalf("setup after delete: %v", m)
	}
	if s := ts.standing(t, testMAC); s != (db.GetMACStandingRow{Registered: 1}) {
		t.Errorf("standing after setup = %+v, want registered with no tombstone", s)
	}
	dev, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil || dev.ID == old.ID {
		t.Errorf("re-registered device = %+v, %v; want a new row", dev, err)
	}
	if m := ts.getJSON(t, "/api/display", displayHeaders(testMAC, newKey)); m["status"] != float64(statusOK) || m["reset_firmware"] != false || m["image_url"] == nil {
		t.Errorf("new key: %v", m)
	}
	// The old key is now a replaced key, which still resets.
	wantReset(t, "old key after setup", ts.getJSON(t, "/api/display", displayHeaders(testMAC, key)))
}

// TestBlockedDisplay checks what /api/display gives a blocked MAC in each
// state it can be in: the blocked screen, never a reset, and nothing saved
// or advanced for a registered device.
func TestBlockedDisplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// prepare sets up the server and returns the request's headers.
		prepare func(t *testing.T, ts *testServer) map[string]string
	}{
		{"registered device with its key", func(t *testing.T, ts *testServer) map[string]string {
			return displayHeaders(testMAC, ts.register(t))
		}},
		{"registered device with no ID", func(t *testing.T, ts *testServer) map[string]string {
			h := displayHeaders(testMAC, ts.register(t))
			delete(h, "ID")
			return h
		}},
		{"registered device with a replaced key", func(t *testing.T, ts *testServer) map[string]string {
			return displayHeaders(testMAC, ts.register(t)+"x")
		}},
		{"deleted device", func(t *testing.T, ts *testServer) map[string]string {
			key := ts.register(t)
			if _, err := device.Delete(t.Context(), ts.db, testMAC, nil, ts.now); err != nil {
				t.Fatal(err)
			}
			return displayHeaders(testMAC, key)
		}},
		{"unregistered MAC", func(*testing.T, *testServer) map[string]string {
			return displayHeaders(testMAC, "nope")
		}},
		{"unregistered MAC with no token", func(*testing.T, *testServer) map[string]string {
			return displayHeaders(testMAC, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			h := tc.prepare(t, ts)
			before := ts.savedState(t)
			if _, err := device.Block(t.Context(), ts.db, testMAC, nil, ts.now); err != nil {
				t.Fatal(err)
			}
			ts.now = ts.now.Add(time.Hour)
			name := ts.wantBlocked(t, "first wake", ts.getJSON(t, "/api/display", h))
			// The screen is the same on the next wake, so the device does not
			// repaint.
			if again := ts.wantBlocked(t, "second wake", ts.getJSON(t, "/api/display", h)); again != name {
				t.Errorf("filename changed from %q to %q", name, again)
			}
			if after := ts.savedState(t); after != before {
				t.Errorf("a blocked request saved state: before %q, after %q", before, after)
			}
		})
	}
}

// TestUnblock checks that an unblock gives a registered device back as it
// was, with its key and no setup, and lets a deleted device's tombstone
// reset it.
func TestUnblock(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	if _, err := device.Block(t.Context(), ts.db, testMAC, nil, ts.now); err != nil {
		t.Fatal(err)
	}
	ts.wantBlocked(t, "blocked", ts.getJSON(t, "/api/display", displayHeaders(testMAC, key)))
	if _, err := device.Unblock(t.Context(), ts.db, testMAC, nil); err != nil {
		t.Fatal(err)
	}
	if m := ts.getJSON(t, "/api/display", displayHeaders(testMAC, key)); m["status"] != float64(statusOK) || m["refresh_rate"] == float64(blockedRefresh) || m["image_url"] == nil {
		t.Errorf("after unblock: %v", m)
	}

	// Delete, then block: the block wins over the tombstone, and an unblock
	// lets the tombstone reset the device.
	if _, err := device.Delete(t.Context(), ts.db, testMAC, nil, ts.now); err != nil {
		t.Fatal(err)
	}
	if _, err := device.Block(t.Context(), ts.db, testMAC, nil, ts.now); err != nil {
		t.Fatal(err)
	}
	ts.wantBlocked(t, "deleted and blocked", ts.getJSON(t, "/api/display", displayHeaders(testMAC, key)))
	if _, err := device.Unblock(t.Context(), ts.db, testMAC, nil); err != nil {
		t.Fatal(err)
	}
	wantReset(t, "deleted and unblocked", ts.getJSON(t, "/api/display", displayHeaders(testMAC, key)))
}

// TestBlockedSetup checks that a blocked MAC gets 404 and the blocked
// screen from /api/setup even while the join window is open, that it is
// given no key, and that a registered device's key still stands.
func TestBlockedSetup(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	before, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	// A third MAC registers, so the window is open.
	if m := ts.getJSON(t, "/api/setup", setupHeaders("D8:3B:DA:00:00:02")); m["status"] != float64(statusSetupOK) {
		t.Fatalf("the window is not open: %v", m)
	}
	for _, mac := range []string{testMAC, strangerMAC} {
		if _, err := device.Block(t.Context(), ts.db, mac, nil, ts.now); err != nil {
			t.Fatal(err)
		}
	}
	for _, mac := range []string{testMAC, strangerMAC} {
		m := ts.getJSON(t, "/api/setup", setupHeaders(mac))
		url, _ := m["image_url"].(string)
		if m["status"] != float64(statusSetupUnknown) || m["api_key"] != nil || m["message"] != "MAC blocked" || url == "" {
			t.Errorf("%s: %v, want 404 with the blocked screen", mac, m)
			continue
		}
		ts.fetchImage(t, url, "")
	}
	if s := ts.standing(t, strangerMAC); s.Registered != 0 {
		t.Error("a blocked MAC was registered")
	}
	after, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil || after.ID != before.ID || !bytes.Equal(after.ApiKeyHash, hashKey(key)) {
		t.Errorf("a refused setup changed the device's key: %+v, %v", after, err)
	}
}

// TestBlockedLog checks that a blocked device's log entries are refused.
func TestBlockedLog(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	if _, err := device.Block(t.Context(), ts.db, testMAC, nil, ts.now); err != nil {
		t.Fatal(err)
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/log", map[string]string{"ID": testMAC, "Access-Token": key}, logBody(1)); code != http.StatusForbidden {
		t.Errorf("blocked log: HTTP %d, want 403", code)
	}
}

// TestDeleteRacesDisplay deletes the device while its /api/display request
// is under way, after the request found the device and before it saved
// what it reported. The request must not bring back any row of the deleted
// device, must not be a reset or an HTTP error, and the device's next
// request must reset it.
func TestDeleteRacesDisplay(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	dev, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	ts.srv.now = func() time.Time {
		// The first time the handler reads the clock is after it found the
		// device.
		once.Do(func() {
			if _, err := device.Delete(t.Context(), ts.db, testMAC, nil, ts.now); err != nil {
				t.Error(err)
			}
		})
		return ts.now.UTC()
	}
	// Saving the device's state fails on its foreign key, and the request
	// gets the answer of an internal failure: keep the screen and come back
	// in a few minutes.
	m := ts.getJSON(t, "/api/display", displayHeaders(testMAC, key))
	if m["status"] != float64(statusOK) || m["reset_firmware"] != false || m["image_url"] != nil || m["refresh_rate"] != float64(retryRefresh) {
		t.Errorf("racing request: %v, want status 0, no image and no reset", m)
	}
	for table, query := range map[string]string{
		"device":         `SELECT COUNT(*) FROM device WHERE id = ?`,
		"device_state":   `SELECT COUNT(*) FROM device_state WHERE device_id = ?`,
		"device_cursor":  `SELECT COUNT(*) FROM device_cursor WHERE device_id = ?`,
		"firmware_offer": `SELECT COUNT(*) FROM firmware_offer WHERE device_id = ?`,
	} {
		var n int
		if err := ts.db.Reader.QueryRowContext(t.Context(), query, dev.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows of the deleted device, %v", table, n, err)
		}
	}
	wantReset(t, "next request", ts.getJSON(t, "/api/display", displayHeaders(testMAC, key)))
}
