package adminui

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
)

const (
	blockMACa = "AA:BB:CC:DD:EE:01"
	blockMACb = "AA:BB:CC:DD:EE:02"
)

// blockMAC blocks the MAC at the test's time.
func blockMAC(t *testing.T, tu *testUI, mac string) {
	t.Helper()
	if _, err := device.Block(t.Context(), tu.db, mac, nil, tu.now); err != nil {
		t.Fatal(err)
	}
}

// isBlocked reports whether the MAC is in the blocklist.
func isBlocked(t *testing.T, tu *testUI, mac string) bool {
	t.Helper()
	rows, err := device.ListBlocked(t.Context(), tu.db.Read)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if rows[i].Mac == mac {
			return true
		}
	}
	return false
}

// addCanaryDevice makes the device with testMAC and the friendly ID canaryID,
// which must be the first device made, able to be the canary of a build
// addFirmware adds.
func addCanaryDevice(t *testing.T, tu *testUI, name string) {
	t.Helper()
	addDevice(t, tu, testMAC, canaryID, name, 0)
	err := tu.db.Write.UpdateDeviceFirmware(t.Context(), db.UpdateDeviceFirmwareParams{FwModel: fwModel, FwVersion: "1.8.17", ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	updates := true
	if _, err := device.Update(t.Context(), tu.db, canaryID, device.Change{UpdatesEnabled: &updates}); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceDeletePage(t *testing.T) {
	t.Parallel()
	const reset = "At its next wake the device is reset: it forgets its key, Wi-Fi settings and server, and starts its setup portal."
	const rollout = "The rollout of 1.8.19 for " + fwModel + " ends with it, so that build is offered to no device until a new rollout names a canary."
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, tu *testUI, dir string)
		want    []string
		notWant []string
	}{
		{
			name:  "plain",
			setup: func(*testing.T, *testUI, string) {},
			want: []string{
				"<h2 id=\"delete-title\">Delete &lt;b&gt;Kitchen&lt;/b&gt;?</h2>", "<dd>ABC123</dd>", "<dd>" + testMAC + "</dd>",
				"The device, its settings, logs, playlist links and firmware offers are deleted.", reset, "It registers as a new device.",
				`<form method="post" action="/devices/ABC123/delete">`, ">Delete device</button>",
				`<a class="btn btn-secondary" href="/devices/ABC123">Keep device</a>`,
			},
			notWant: []string{"linked to", "not reset while", "rollout", "<b>"},
		},
		{
			name:    "blocked",
			setup:   func(t *testing.T, tu *testUI, _ string) { blockMAC(t, tu, testMAC) },
			want:    []string{"It is blocked, so it is not reset while the block lasts. Unblocking it resets it at its next check, within a day."},
			notWant: []string{reset},
		},
		{
			name: "one playlist",
			setup: func(t *testing.T, tu *testUI, _ string) {
				linkTo(t, tu, "ABC123", newPlaylist(t, tu, "Morning"))
			},
			want: []string{"It is linked to 1 playlist."},
		},
		{
			name: "two playlists",
			setup: func(t *testing.T, tu *testUI, _ string) {
				linkTo(t, tu, "ABC123", newPlaylist(t, tu, "Morning"), newPlaylist(t, tu, "Evening"))
			},
			want: []string{"It is linked to 2 playlists."},
		},
		{
			name: "canary of an active rollout",
			setup: func(t *testing.T, tu *testUI, dir string) {
				startRollout(t, tu, addFirmware(t, tu, dir, fwModel).ID)
			},
			want: []string{rollout, reset},
		},
		{
			name: "canary of a canceled rollout",
			setup: func(t *testing.T, tu *testUI, dir string) {
				fw := addFirmware(t, tu, dir, fwModel)
				startRollout(t, tu, fw.ID)
				if err := firmware.CancelRollout(t.Context(), tu.db, fw.ID); err != nil {
					t.Fatal(err)
				}
			},
			notWant: []string{"rollout of"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, dir := newFirmwareUI(t)
			addCanaryDevice(t, tu, "<b>Kitchen</b>")
			tc.setup(t, tu, dir)
			w := tu.get(t, tu.signIn(t), "/devices/ABC123/delete")
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			body := w.Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(body, bad) {
					t.Errorf("page has %q:\n%s", bad, body)
				}
			}
		})
	}
}

// seenRE matches a hidden field of the standing a page shows.
var seenRE = regexp.MustCompile(`<input type="hidden" name="(seen_[a-z]+)" value="([^"]*)">`)

// seenOf returns the standing fields a page's form would post.
func seenOf(t *testing.T, body string) url.Values {
	t.Helper()
	out := url.Values{}
	for _, m := range seenRE.FindAllStringSubmatch(body, -1) {
		out.Set(m[1], m[2])
	}
	if len(out) != 5 {
		t.Fatalf("page has %d standing fields, want 5:\n%s", len(out), body)
	}
	return out
}

// deleteNow deletes the device with testMAC outside the page under test.
func deleteNow(t *testing.T, tu *testUI) {
	t.Helper()
	if _, err := device.Delete(t.Context(), tu.db, testMAC, nil, tu.now); err != nil {
		t.Fatal(err)
	}
}

// hasDevice reports whether a device is registered.
func hasDevice(t *testing.T, tu *testUI, ref string) bool {
	t.Helper()
	_, err := device.FindDevice(t.Context(), tu.db.Read, ref)
	if err != nil && !errors.Is(err, device.ErrNoDevice) {
		t.Fatal(err)
	}
	return err == nil
}

func TestDeviceDelete(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	addDevice(t, tu, "D8:3B:DA:00:00:02", "XYZ789", "Den", 0)
	c := tu.signIn(t)
	form := seenOf(t, tu.get(t, c, "/devices/ABC123/delete").Body.String())
	if got := form.Get("seen_mac") + " " + form.Get("seen_blocked") + " " + form.Get("seen_deleted") + " " + form.Get("seen_canary"); got != testMAC+" false false " {
		t.Errorf("standing on the page = %q", got)
	}
	// A page for a device that is gone is not found, and a post that finds
	// nothing deletes nothing.
	for _, tc := range []struct {
		name, path string
		form       url.Values
		status     int
		location   string
		body       string
	}{
		{"no standing", "/devices/ABC123/delete", nil, http.StatusBadRequest, "", reloadText},
		{"bad standing", "/devices/ABC123/delete", url.Values{"seen_mac": {testMAC}, "seen_device": {"x"}}, http.StatusBadRequest, "", reloadText},
		{"first post", "/devices/ABC123/delete", form, http.StatusSeeOther, "/devices", ""},
		{"second post", "/devices/ABC123/delete", form, http.StatusNotFound, "", "No such device"},
		{"unknown device", "/devices/NOPE00/delete", nil, http.StatusNotFound, "", "No such device"},
	} {
		w := tu.post(t, c, tc.path, tc.form)
		if w.Code != tc.status || w.Header().Get("Location") != tc.location {
			t.Errorf("%s: status %d to %q, want %d to %q", tc.name, w.Code, w.Header().Get("Location"), tc.status, tc.location)
		}
		if !strings.Contains(w.Body.String(), tc.body) {
			t.Errorf("%s: page lacks %q: %s", tc.name, tc.body, w.Body)
		}
		if tc.status == http.StatusBadRequest && !hasDevice(t, tu, "ABC123") {
			t.Fatalf("%s: the device was deleted", tc.name)
		}
	}
	if w := tu.get(t, c, "/devices/ABC123/delete"); w.Code != http.StatusNotFound {
		t.Errorf("confirm page of a deleted device: status %d", w.Code)
	}
	list := tu.get(t, c, "/devices").Body.String()
	if strings.Contains(list, "Kitchen") || !strings.Contains(list, "Den") {
		t.Errorf("the list should have only the other device:\n%s", list)
	}
	// The device's MAC is a tombstone, which a reset answers. It is not
	// blocked.
	if isBlocked(t, tu, testMAC) {
		t.Error("a delete blocked the MAC")
	}
}

// TestDeviceDeleteStale checks that a delete from a confirm page that no
// longer says what the delete would do changes nothing.
func TestDeviceDeleteStale(t *testing.T) {
	t.Parallel()
	const reissuedMAC = "D8:3B:DA:00:00:09"
	for _, tc := range []struct {
		name string
		// change is what happens after the page loaded.
		change func(t *testing.T, tu *testUI, dir string)
		status int
		// body is on the page the post gets, and kept is the device that
		// must still be registered.
		body []string
		kept string
	}{
		{
			name:   "blocked since",
			change: func(t *testing.T, tu *testUI, _ string) { blockMAC(t, tu, testMAC) },
			status: http.StatusConflict,
			body:   []string{`role="alert">` + deviceChanged, "It is blocked, so it is not reset while the block lasts.", `name="seen_blocked" value="true"`},
			kept:   testMAC,
		},
		{
			name: "became a canary since",
			change: func(t *testing.T, tu *testUI, dir string) {
				startRollout(t, tu, addFirmware(t, tu, dir, fwModel).ID)
			},
			status: http.StatusConflict,
			body:   []string{deviceChanged, "The rollout of 1.8.19 for " + fwModel + " ends with it", `name="seen_canary" value="1"`},
			kept:   testMAC,
		},
		{
			name: "deleted and registered again since",
			change: func(t *testing.T, tu *testUI, _ string) {
				deleteNow(t, tu)
				addDevice(t, tu, testMAC, "NEW456", "Again", 0)
			},
			status: http.StatusConflict,
			body:   []string{deviceChanged, `<h2 id="delete-title">Delete Again?</h2>`, `action="/devices/NEW456/delete"`},
			kept:   testMAC,
		},
		{
			name: "friendly ID reissued to a newer device",
			change: func(t *testing.T, tu *testUI, _ string) {
				deleteNow(t, tu)
				addDevice(t, tu, reissuedMAC, canaryID, "Newer", 0)
			},
			status: http.StatusNotFound,
			body:   []string{"No such device"},
			kept:   reissuedMAC,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, dir := newFirmwareUI(t)
			addCanaryDevice(t, tu, "Kitchen")
			c := tu.signIn(t)
			form := seenOf(t, tu.get(t, c, "/devices/"+canaryID+"/delete").Body.String())
			tc.change(t, tu, dir)
			w := tu.post(t, c, "/devices/"+canaryID+"/delete", form)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			for _, want := range tc.body {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("page lacks %q:\n%s", want, w.Body)
				}
			}
			if !hasDevice(t, tu, tc.kept) {
				t.Errorf("device %s was deleted", tc.kept)
			}
			if tc.status == http.StatusConflict {
				// The page the refusal sent is current, so its form deletes.
				again := tu.post(t, c, "/devices/"+canaryID+"/delete", seenOf(t, w.Body.String()))
				if again.Code != http.StatusSeeOther || hasDevice(t, tu, testMAC) {
					t.Errorf("post of the current page: status %d, device kept %v", again.Code, hasDevice(t, tu, testMAC))
				}
			}
		})
	}
}

func TestDeviceBlockPost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		path    string
		blocked bool // whether the device is blocked when the page loads
		action  string
		// noSeen leaves the standing out of the post, and change is what
		// happens after the page loaded.
		noSeen      bool
		change      func(t *testing.T, tu *testUI)
		status      int
		location    string
		wantBlocked bool
		body        string
	}{
		{name: "block", path: "/devices/ABC123/block", action: "block", status: 303, location: "/devices/ABC123#remove", wantBlocked: true},
		{name: "unblock", path: "/devices/ABC123/block", blocked: true, action: "unblock", status: 303, location: "/devices/ABC123#remove"},
		{name: "by MAC", path: "/devices/" + testMAC + "/block", action: "block", status: 303, location: "/devices/" + testMAC + "#remove", wantBlocked: true},
		{
			name: "block when blocked since", path: "/devices/ABC123/block", action: "block",
			change: func(t *testing.T, tu *testUI) { blockMAC(t, tu, testMAC) },
			status: 409, wantBlocked: true, body: deviceChanged,
		},
		{
			name: "unblock when unblocked since", path: "/devices/ABC123/block", blocked: true, action: "unblock",
			change: func(t *testing.T, tu *testUI) {
				if _, err := device.Unblock(t.Context(), tu.db, testMAC, nil); err != nil {
					t.Fatal(err)
				}
			},
			status: 409, body: "This device is no longer blocked.",
		},
		{
			name: "block when deleted since", path: "/devices/ABC123/block", action: "block",
			change: func(t *testing.T, tu *testUI) { deleteNow(t, tu) },
			status: 404, body: "No such device",
		},
		{name: "bad action", path: "/devices/ABC123/block", action: "delete", status: 400, body: "Choose Block or Unblock."},
		{name: "no action", path: "/devices/ABC123/block", blocked: true, status: 400, wantBlocked: true, body: "Choose Block or Unblock."},
		{name: "no standing", path: "/devices/ABC123/block", action: "block", noSeen: true, status: 400, body: reloadText},
		{name: "unknown device", path: "/devices/NOPE00/block", action: "block", noSeen: true, status: 404, body: "No such device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			if tc.blocked {
				blockMAC(t, tu, testMAC)
			}
			c := tu.signIn(t)
			form := url.Values{}
			if !tc.noSeen {
				form = seenOf(t, tu.get(t, c, "/devices/ABC123").Body.String())
			}
			if tc.action != "" {
				form.Set("action", tc.action)
			}
			if tc.change != nil {
				tc.change(t, tu)
			}
			w := tu.post(t, c, tc.path, form)
			if w.Code != tc.status || w.Header().Get("Location") != tc.location {
				t.Fatalf("status %d to %q, want %d to %q: %s", w.Code, w.Header().Get("Location"), tc.status, tc.location, w.Body)
			}
			if tc.body != "" && !strings.Contains(w.Body.String(), tc.body) {
				t.Errorf("page lacks %q:\n%s", tc.body, w.Body)
			}
			if tc.status == 409 || tc.status == 400 {
				// The message is in the Remove panel, which the page also has.
				wantInOrder(t, w.Body.String(), `id="remove"`, `role="alert">`+tc.body)
			}
			if got := isBlocked(t, tu, testMAC); got != tc.wantBlocked {
				t.Errorf("blocked = %v, want %v", got, tc.wantBlocked)
			}
		})
	}
}

func TestDeviceRemovePanel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		blocked bool
		want    []string
		notWant []string
	}{
		{
			name: "not blocked",
			want: []string{
				`<section class="panel" id="remove"`, "A blocked device shows a \"Blocked\" screen and checks back once a day. Unblocking restores it as it was.",
				`name="action" value="block">Block device</button>`, `<form method="post" action="/devices/ABC123/block#remove">`,
				`<a class="btn btn-secondary btn-danger" href="/devices/ABC123/delete">Delete device…</a>`, `<span class="badge badge-ok">Online</span>`,
			},
			notWant: []string{"Unblock device", "Blocked</span>"},
		},
		{
			name: "blocked", blocked: true,
			want: []string{
				"This device is blocked. It shows a \"Blocked\" screen and checks back once a day. Unblocking restores it as it was at its next check, within a day, with no setup.",
				`name="action" value="unblock">Unblock device</button>`, `<span class="badge badge-warn">Blocked</span>`,
			},
			notWant: []string{">Block device<", "Online</span>"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", tu.now.Unix()-60)
			if tc.blocked {
				blockMAC(t, tu, testMAC)
			}
			c := tu.signIn(t)
			for _, path := range []string{"/devices/ABC123", "/devices"} {
				body := tu.get(t, c, path).Body.String()
				for _, want := range tc.want {
					// Only the device page has the panel, and both have the badge.
					if (strings.Contains(want, "badge") || path == "/devices/ABC123") && !strings.Contains(body, want) {
						t.Errorf("GET %s lacks %q:\n%s", path, want, body)
					}
				}
				for _, bad := range tc.notWant {
					if strings.Contains(body, bad) {
						t.Errorf("GET %s has %q", path, bad)
					}
				}
			}
		})
	}
}

func TestBlockedDevicesPanel(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		tu := newTestUI(t, Config{}, true)
		body := tu.get(t, tu.signIn(t), "/devices").Body.String()
		wantInOrder(t, body, "No devices yet", `id="blocked"`, "No MAC is blocked.", `<form method="post" action="/blocklist#blocked">`,
			`<label for="mac">MAC address</label>`, `autocomplete="off" autocapitalize="characters" spellcheck="false" placeholder="AA:BB:CC:DD:EE:FF"`,
			`A blocked device shows a "Blocked" screen and cannot register, even while the join window is open.`, `type="submit">Block</button>`)
		if strings.Contains(body, "aria-invalid") {
			t.Error("the input is marked invalid with no error")
		}
	})
	t.Run("list", func(t *testing.T) {
		t.Parallel()
		tu := newTestUI(t, Config{}, true)
		addDevice(t, tu, testMAC, "ABC123", "<b>Kitchen</b>", 0)
		addDevice(t, tu, "D8:3B:DA:00:00:02", "XYZ789", "", 0)
		blockMAC(t, tu, testMAC)
		blockMAC(t, tu, "D8:3B:DA:00:00:02")
		blockMAC(t, tu, blockMACa)
		body := tu.get(t, tu.signIn(t), "/devices").Body.String()
		if strings.Contains(body, "<b>") || strings.Contains(body, "No MAC is blocked.") {
			t.Errorf("a name was not escaped, or the empty state is shown:\n%s", body)
		}
		wantInOrder(t, body, `id="blocked"`,
			`<span class="item-name">`+blockMACa+`</span>`, `<span class="rel">Not registered</span>`, `<time datetime="1970-01-12T13:46:40Z">`,
			`<a class="btn btn-secondary" href="/blocklist/`+blockMACa+`" aria-label="Unblock `+blockMACa+`…">Unblock…</a>`,
			`<span class="item-name">`+testMAC+`</span>`, `<a href="/devices/ABC123">&lt;b&gt;Kitchen&lt;/b&gt;</a>`,
			`<span class="item-name">D8:3B:DA:00:00:02</span>`, `<a href="/devices/XYZ789">XYZ789</a>`)
	})
}

// TestBlockedPanelDeletedDevice checks that a blocked MAC whose device was
// deleted says so, as its unblock page does.
func TestBlockedPanelDeletedDevice(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	blockMAC(t, tu, testMAC)
	blockMAC(t, tu, blockMACa)
	deleteNow(t, tu)
	body := tu.get(t, tu.signIn(t), "/devices").Body.String()
	wantInOrder(t, body, `<span class="item-name">`+blockMACa+`</span>`, `<span class="rel">Not registered</span>`,
		`<span class="item-name">`+testMAC+`</span>`, `<span class="rel">Deleted device</span>`)
}

// TestRemovePanelCanary checks that the Remove panel names a rollout that
// blocking its canary holds, and only one the canary has not finished.
func TestRemovePanelCanary(t *testing.T) {
	t.Parallel()
	const before = "This device is the canary of the rollout of 1.8.19 for " + fwModel + " and has not reported that version. While it is blocked, that build is offered to no device."
	const held = "The rollout of 1.8.19 for " + fwModel + " waits while this device, its canary, is blocked. That build is offered to no device."
	for _, tc := range []struct {
		name string
		// rollout is whether the device is a canary, canceled whether that
		// rollout is canceled, and blocked whether the device is blocked.
		rollout, canceled, blocked bool
		want                       string
	}{
		{name: "no rollout"},
		{name: "canary", rollout: true, want: before},
		{name: "blocked canary", rollout: true, blocked: true, want: held},
		{name: "canary of a canceled rollout", rollout: true, canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, dir := newFirmwareUI(t)
			addCanaryDevice(t, tu, "Kitchen")
			if tc.rollout {
				fw := addFirmware(t, tu, dir, fwModel)
				startRollout(t, tu, fw.ID)
				if tc.canceled {
					if err := firmware.CancelRollout(t.Context(), tu.db, fw.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.blocked {
				blockMAC(t, tu, testMAC)
			}
			body := tu.get(t, tu.signIn(t), "/devices/"+canaryID).Body.String()
			for _, note := range []string{before, held} {
				if got := strings.Contains(body, note); got != (note == tc.want) {
					t.Errorf("page has %q = %v, want %v", note, got, note == tc.want)
				}
			}
		})
	}
}

func TestBlockMACPost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		blocked   []string
		mac       string
		status    int
		location  string
		wantAfter []string // blocked MACs after
		body      []string
		notBody   []string
	}{
		{name: "a new MAC", mac: blockMACa, status: 303, location: "/devices#blocked", wantAfter: []string{blockMACa}},
		{name: "a lower case MAC", mac: " aa-bb-cc-dd-ee-01 ", status: 303, location: "/devices#blocked", wantAfter: []string{blockMACa}},
		{name: "one already blocked", blocked: []string{blockMACa}, mac: blockMACa, status: 303, location: "/devices#blocked", wantAfter: []string{blockMACa}},
		{name: "a registered device's MAC", mac: testMAC, status: 303, location: "/devices#blocked", wantAfter: []string{testMAC}},
		{
			name: "bad MAC", mac: `<i>nope</i>`, status: 400,
			body:    []string{`id="mac-error" role="alert">Enter a MAC address, such as AA:BB:CC:DD:EE:FF.`, `value="&lt;i&gt;nope&lt;/i&gt;"`, `aria-invalid="true" aria-describedby="mac-error mac-hint"`},
			notBody: []string{"<i>"},
		},
		{name: "friendly ID", mac: "ABC123", status: 400, body: []string{"Enter a MAC address, such as AA:BB:CC:DD:EE:FF.", `value="ABC123"`}},
		{name: "empty MAC", status: 400, body: []string{"Enter a MAC address"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			for _, mac := range tc.blocked {
				blockMAC(t, tu, mac)
			}
			w := tu.post(t, tu.signIn(t), "/blocklist", url.Values{"mac": {tc.mac}})
			if w.Code != tc.status || w.Header().Get("Location") != tc.location {
				t.Fatalf("status %d to %q, want %d to %q: %s", w.Code, w.Header().Get("Location"), tc.status, tc.location, w.Body)
			}
			body := w.Body.String()
			for _, want := range tc.body {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
			for _, bad := range tc.notBody {
				if strings.Contains(body, bad) {
					t.Errorf("page has %q", bad)
				}
			}
			if tc.status >= 400 && !strings.Contains(body, `id="blocked"`) {
				t.Error("the error is not on the devices page")
			}
			if got := blockedMACs(t, tu); strings.Join(got, ",") != strings.Join(tc.wantAfter, ",") {
				t.Errorf("blocked = %v, want %v", got, tc.wantAfter)
			}
		})
	}
}

// blockedMACs returns the blocked MACs in order.
func blockedMACs(t *testing.T, tu *testUI) []string {
	t.Helper()
	rows, err := device.ListBlocked(t.Context(), tu.db.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(rows))
	for i := range rows {
		got = append(got, rows[i].Mac)
	}
	return got
}

// The three things an unblock can do, each of which its page says.
const (
	unblockRestores = "At its next check, within a day, it is back as it was, with its key and no setup."
	unblockResets   = "This MAC belonged to a device that was deleted. Once unblocked, the device is reset at its next check, within a day"
	unblockUnknown  = "No device with this MAC is registered. Once unblocked, it can register while the join window is open."
)

func TestUnblockPage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, tu *testUI)
		path   string
		status int
		want   []string
	}{
		{
			name:  "registered device",
			setup: func(t *testing.T, tu *testUI) { blockMAC(t, tu, testMAC) },
			path:  "/blocklist/" + testMAC, status: 200,
			want: []string{
				`<h2 id="unblock-title">Unblock ` + testMAC + `?</h2>`, `<a href="/devices/ABC123">&lt;b&gt;Kitchen&lt;/b&gt;</a> has this MAC. ` + unblockRestores,
				`<form method="post" action="/blocklist/` + testMAC + `#blocked">`, `name="seen_blocked" value="true"`, `name="seen_deleted" value="false"`,
				`type="submit">Unblock</button>`, `<a class="btn btn-secondary" href="/devices#blocked">Keep blocked</a>`,
			},
		},
		{
			name: "deleted device",
			setup: func(t *testing.T, tu *testUI) {
				blockMAC(t, tu, testMAC)
				deleteNow(t, tu)
			},
			path: "/blocklist/" + testMAC, status: 200,
			want: []string{unblockResets, `name="seen_device" value="0"`, `name="seen_deleted" value="true"`},
		},
		{
			name:  "MAC that never registered, in lower case",
			setup: func(t *testing.T, tu *testUI) { blockMAC(t, tu, blockMACa) },
			path:  "/blocklist/aa:bb:cc:dd:ee:01", status: 200,
			want: []string{unblockUnknown, `action="/blocklist/` + blockMACa + `#blocked"`},
		},
		{name: "not blocked", setup: func(*testing.T, *testUI) {}, path: "/blocklist/" + testMAC, status: 404, want: []string{"That MAC is not blocked"}},
		{name: "not a MAC", setup: func(*testing.T, *testUI) {}, path: "/blocklist/ABC123", status: 404, want: []string{"That MAC is not blocked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "<b>Kitchen</b>", 0)
			tc.setup(t, tu)
			w := tu.get(t, tu.signIn(t), tc.path)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			body := w.Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
			if strings.Contains(body, "<b>") {
				t.Error("a name was not escaped")
			}
		})
	}
}

func TestUnblockPost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// noSeen leaves the standing out of the post, and change is what
		// happens after the page loaded.
		noSeen      bool
		change      func(t *testing.T, tu *testUI)
		status      int
		location    string
		wantBlocked bool
		body        []string
	}{
		{name: "unblock", status: 303, location: "/devices#blocked"},
		{name: "no standing", noSeen: true, status: 400, wantBlocked: true, body: []string{`role="alert">` + reloadText, unblockRestores}},
		{
			name:   "deleted since, so the unblock would reset it",
			change: func(t *testing.T, tu *testUI) { deleteNow(t, tu) },
			status: 409, wantBlocked: true, body: []string{"This MAC&#39;s device changed after the page loaded, so nothing was done.", unblockResets},
		},
		{
			name: "unblocked since",
			change: func(t *testing.T, tu *testUI) {
				if _, err := device.Unblock(t.Context(), tu.db, testMAC, nil); err != nil {
					t.Fatal(err)
				}
			},
			status: 409, body: []string{`role="alert">That MAC is no longer blocked.`, `id="blocked"`, "No MAC is blocked."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			blockMAC(t, tu, testMAC)
			c := tu.signIn(t)
			form := url.Values{}
			if !tc.noSeen {
				form = seenOf(t, tu.get(t, c, "/blocklist/"+testMAC).Body.String())
			}
			if tc.change != nil {
				tc.change(t, tu)
			}
			w := tu.post(t, c, "/blocklist/"+testMAC, form)
			if w.Code != tc.status || w.Header().Get("Location") != tc.location {
				t.Fatalf("status %d to %q, want %d to %q: %s", w.Code, w.Header().Get("Location"), tc.status, tc.location, w.Body)
			}
			for _, want := range tc.body {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("page lacks %q:\n%s", want, w.Body)
				}
			}
			if got := isBlocked(t, tu, testMAC); got != tc.wantBlocked {
				t.Errorf("blocked = %v, want %v", got, tc.wantBlocked)
			}
		})
	}
	t.Run("standing of another MAC", func(t *testing.T) {
		t.Parallel()
		tu := newTestUI(t, Config{}, true)
		blockMAC(t, tu, blockMACa)
		blockMAC(t, tu, blockMACb)
		c := tu.signIn(t)
		form := seenOf(t, tu.get(t, c, "/blocklist/"+blockMACa).Body.String())
		if w := tu.post(t, c, "/blocklist/"+blockMACb, form); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reloadText) {
			t.Errorf("status %d: %s", w.Code, w.Body)
		}
		if got := blockedMACs(t, tu); len(got) != 2 {
			t.Errorf("blocked = %v, want both", got)
		}
	})
}
