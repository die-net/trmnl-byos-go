package adminapi

import (
	"strings"
	"testing"
	"time"
)

// devicesList returns GET /api/v1/devices.
func (ta *testAPI) devicesList(t *testing.T) []deviceJSON {
	t.Helper()
	var list []deviceJSON
	ta.call(t, "GET", "/api/v1/devices", "", 200, &list)
	return list
}

// blockList returns GET /api/v1/blocklist.
func (ta *testAPI) blockList(t *testing.T) []blockedJSON {
	t.Helper()
	var list []blockedJSON
	w := ta.call(t, "GET", "/api/v1/blocklist", "", 200, &list)
	if list == nil {
		t.Fatalf("blocklist is not an array: %s", w.Body)
	}
	return list
}

func TestDeleteDevice(t *testing.T) {
	t.Parallel()
	for _, id := range []string{testFriend, testMAC} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			ta := newTestAPI(t, false)
			ta.addDevice(t)
			if w := ta.call(t, "DELETE", "/api/v1/devices/"+id, "", 204, nil); w.Body.Len() != 0 {
				t.Errorf("body %q", w.Body)
			}
			if list := ta.devicesList(t); len(list) != 0 {
				t.Errorf("devices after delete: %+v", list)
			}
			ta.wantError(t, "GET", "/api/v1/devices/"+testFriend, "", 404, "no such device")
			ta.wantError(t, "DELETE", "/api/v1/devices/"+id, "", 404, "no such device")
		})
	}
}

func TestBlocklist(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ta.addDevice(t)
	const stranger = "AA:BB:CC:DD:EE:FF"
	if list := ta.blockList(t); len(list) != 0 {
		t.Fatalf("blocklist at the start: %+v", list)
	}
	if list := ta.devicesList(t); len(list) != 1 || list[0].Blocked {
		t.Fatalf("devices at the start: %+v", list)
	}

	// Block by friendly ID, and an unregistered MAC, which has colons that
	// the mux must pass through {id}.
	var b blockedJSON
	ta.call(t, "PUT", "/api/v1/blocklist/"+testFriend, "", 200, &b)
	if b.MAC != testMAC || b.BlockedAt != ta.now.Unix() || b.Device == nil || *b.Device != testFriend || b.Name == nil || *b.Name != "" {
		t.Errorf("block by friendly ID: %+v", b)
	}
	first := ta.now.Unix()
	ta.now = ta.now.Add(time.Hour)
	ta.call(t, "PUT", "/api/v1/blocklist/"+strings.ToLower(stranger), "", 200, &b)
	if b.MAC != stranger || b.BlockedAt != ta.now.Unix() || b.Device != nil || b.Name != nil {
		t.Errorf("block by unregistered MAC: %+v", b)
	}

	// A second block keeps the first time.
	ta.now = ta.now.Add(time.Hour)
	ta.call(t, "PUT", "/api/v1/blocklist/"+testMAC, "", 200, &b)
	if b.MAC != testMAC || b.BlockedAt != first {
		t.Errorf("second block: %+v, want blocked_at %d", b, first)
	}

	list := ta.blockList(t)
	if len(list) != 2 || list[0].MAC != stranger || list[0].Device != nil || list[0].Name != nil ||
		list[1].MAC != testMAC || list[1].BlockedAt != first || list[1].Device == nil || *list[1].Device != testFriend {
		t.Errorf("blocklist: %+v", list)
	}
	if devs := ta.devicesList(t); len(devs) != 1 || !devs[0].Blocked {
		t.Errorf("devices after block: %+v", devs)
	}
	var one deviceJSON
	ta.call(t, "GET", "/api/v1/devices/"+testFriend, "", 200, &one)
	if !one.Blocked {
		t.Errorf("device after block: %+v", one)
	}

	// Unblock by MAC and by friendly ID, then again.
	ta.call(t, "DELETE", "/api/v1/blocklist/"+stranger, "", 204, nil)
	ta.call(t, "DELETE", "/api/v1/blocklist/"+testFriend, "", 204, nil)
	ta.wantError(t, "DELETE", "/api/v1/blocklist/"+testFriend, "", 404, "not blocked")
	ta.wantError(t, "DELETE", "/api/v1/blocklist/"+stranger, "", 404, "not blocked")
	if list := ta.blockList(t); len(list) != 0 {
		t.Errorf("blocklist after unblock: %+v", list)
	}
	if devs := ta.devicesList(t); len(devs) != 1 || devs[0].Blocked {
		t.Errorf("devices after unblock: %+v", devs)
	}
}

func TestBlocklistBadID(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ta.addDevice(t)
	for _, method := range []string{"PUT", "DELETE"} {
		for _, id := range []string{"ZZZ999", "AA:BB:CC", "nonsense"} {
			ta.wantError(t, method, "/api/v1/blocklist/"+id, "", 404, "no such device")
		}
	}
	if list := ta.blockList(t); len(list) != 0 {
		t.Errorf("blocklist after refusals: %+v", list)
	}
}

func TestBlocklistNeedsToken(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	for _, r := range []struct{ method, path string }{
		{"DELETE", "/api/v1/devices/" + testFriend},
		{"GET", "/api/v1/blocklist"},
		{"PUT", "/api/v1/blocklist/" + testMAC},
		{"DELETE", "/api/v1/blocklist/" + testMAC},
	} {
		if w := ta.do(t, r.method, r.path, nil, "Authorization", ""); w.Code != 401 {
			t.Errorf("%s %s without a token: %d, want 401", r.method, r.path, w.Code)
		}
	}
}
