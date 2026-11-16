package device

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

const testMAC = "D8:3B:DA:12:34:56"

func TestCalibrate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	now := time.Unix(1_800_000_000, 0)
	_, err = client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: testMAC, ApiKeyHash: []byte{1}, FriendlyID: "ABC123", CreatedAt: now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		device string
		dot    int
		window time.Duration
		want   error
	}{
		{"dot too small", testMAC, 0, time.Minute, errkind.ErrInvalid},
		{"dot too big", testMAC, 3, time.Minute, errkind.ErrInvalid},
		{"no time", testMAC, 1, 0, errkind.ErrInvalid},
		{"negative time", testMAC, 1, -time.Minute, errkind.ErrInvalid},
		{"unknown device", "ZZZ999", 1, time.Minute, ErrNoDevice},
		{"unknown device is not found", "ZZZ999", 1, time.Minute, errkind.ErrNotFound},
	} {
		if _, _, err := Calibrate(ctx, client, tc.device, tc.dot, tc.window, now); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	dev, err := client.Read.GetDeviceByMAC(ctx, testMAC)
	if err != nil || dev.ScreenOverride != "" {
		t.Fatalf("a refused request changed the device: %+v, %v", dev, err)
	}
	_, until, err := Calibrate(ctx, client, "abc123", 2, 10*time.Minute, now)
	if err != nil || !until.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("Calibrate = %v, %v", until, err)
	}
	if dev, err = client.Read.GetDeviceByMAC(ctx, testMAC); err != nil || dev.ScreenOverride != ScreenCalibrate2x2 || dev.ScreenOverrideUntil != until.Unix() {
		t.Errorf("device = %+v, %v", dev, err)
	}
}

func TestStatusHealth(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	i := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		name     string
		lastSeen *int64
		refresh  *int64
		want     Health
	}{
		{"never seen", nil, i(900), NeverSeen},
		{"never seen, no interval", nil, nil, NeverSeen},
		{"just now", i(now.Unix()), i(900), Online},
		{"exactly twice the interval", i(now.Unix() - 1800), i(900), Online},
		{"one second past twice", i(now.Unix() - 1801), i(900), Late},
		{"long ago", i(now.Unix() - 86400), i(900), Late},
		{"no interval recorded", i(now.Unix()), nil, Late},
		{"clock ahead of the device", i(now.Unix() + 60), i(900), Online},
	} {
		if got := StatusHealth(tc.lastSeen, tc.refresh, now); got != tc.want {
			t.Errorf("%s: StatusHealth = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestListStatus(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	rows, err := ListStatus(ctx, client.Read)
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty ListStatus = %v, %v", rows, err)
	}
	for _, mac := range []string{"D8:3B:DA:00:00:01", "D8:3B:DA:00:00:02"} {
		dev, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: mac, ApiKeyHash: []byte(mac), FriendlyID: mac[len(mac)-2:] + "X", CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		if mac[len(mac)-1] == '2' {
			err = client.Write.UpsertDeviceState(ctx, db.UpsertDeviceStateParams{DeviceID: dev.ID, LastSeen: 100, LastRefreshRateSent: 900})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err = ListStatus(ctx, client.Read)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ListStatus = %d rows, %v", len(rows), err)
	}
	var seen, unseen int
	for _, r := range rows {
		if r.LastSeen != nil {
			seen++
		} else {
			unseen++
		}
	}
	if seen != 1 || unseen != 1 {
		t.Errorf("%d seen and %d unseen devices, want one of each", seen, unseen)
	}
}

func TestCleanName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in, want string
		invalid        bool
	}{
		{"plain", "Kitchen", "Kitchen", false},
		{"trimmed", " \tKitchen \n", "Kitchen", false},
		{"empty", "", "", false},
		{"only space", "   ", "", false},
		{"longest", strings.Repeat("x", MaxNameBytes), strings.Repeat("x", MaxNameBytes), false},
		{"multibyte", "Küche", "Küche", false},
		{"too long", strings.Repeat("x", MaxNameBytes+1), "", true},
		{"too long in bytes", strings.Repeat("é", MaxNameBytes/2+1), "", true},
		{"newline inside", "a\nb", "", true},
		{"bad UTF-8", "a\xffb", "", true},
	} {
		got, err := CleanName(tc.in)
		if tc.invalid != errors.Is(err, errkind.ErrInvalid) || got != tc.want {
			t.Errorf("%s: CleanName(%q) = %q, %v; want %q, invalid %v", tc.name, tc.in, got, err, tc.want, tc.invalid)
		}
	}
}

// TestUpdate checks each setting Update takes, what it refuses, that a
// refused change changes nothing, even its valid fields, and that nil
// fields are kept. The device reports the OG DIY kit's Model, which is
// og_plus, with the palettes bw and gray-4.
func TestUpdate(t *testing.T) {
	t.Parallel()
	s := func(v string) *string { return &v }
	on := true
	for _, tc := range []struct {
		name   string
		device string
		change Change
		want   error
		// check is the device after the change, when it is accepted.
		check func(dev db.Device) bool
	}{
		{"name, trimmed", "abc123", Change{Name: s(" Kitchen ")}, nil, func(d db.Device) bool { return d.Name == "Kitchen" && d.FwPin == "1.8.1" }},
		{"name too long", testMAC, Change{Name: s(strings.Repeat("x", 65))}, errkind.ErrInvalid, nil},
		{"unknown device", "ZZZ999", Change{Name: s("x")}, ErrNoDevice, nil},
		{"policy", testMAC, Change{UpdatesEnabled: &on, FwPin: s("")}, nil, func(d db.Device) bool { return d.UpdatesEnabled == 1 && d.FwPin == "" && d.Name == "Old" }},
		{"profile", testMAC, Change{Profile: s("og_bwry")}, nil, func(d db.Device) bool { return d.ProfileOverride == "og_bwry" }},
		{"profile not in the registry", testMAC, Change{Name: s("New"), Profile: s("og_nope")}, errkind.ErrInvalid, nil},
		{"palette of the reported model", testMAC, Change{Palette: s("gray-4")}, nil, func(d db.Device) bool { return d.PaletteOverride == "gray-4" }},
		{"palette the model lacks", testMAC, Change{Name: s("New"), Palette: s("gray-16")}, errkind.ErrInvalid, nil},
		{"palette of the new profile", testMAC, Change{Profile: s("v2"), Palette: s("gray-16")}, nil, func(d db.Device) bool {
			return d.ProfileOverride == "v2" && d.PaletteOverride == "gray-16"
		}},
		{"profile that leaves the stored palette out", testMAC, Change{Profile: s("og_bwry")}, errkind.ErrInvalid, nil},
		{"clear both", testMAC, Change{Profile: s(""), Palette: s("")}, nil, func(d db.Device) bool { return d.ProfileOverride == "" && d.PaletteOverride == "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			dev, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: testMAC, ApiKeyHash: []byte{1}, FriendlyID: "ABC123", Name: "Old", FwModel: "xiao_epaper_display", CreatedAt: 1})
			if err != nil {
				t.Fatal(err)
			}
			palette := ""
			if tc.name == "profile that leaves the stored palette out" {
				palette = "gray-4"
			}
			err = client.Write.UpdateDeviceSettings(ctx, db.UpdateDeviceSettingsParams{Name: "Old", FwPin: "1.8.1", PaletteOverride: palette, ID: dev.ID})
			if err != nil {
				t.Fatal(err)
			}
			before, err := client.Read.GetDevice(ctx, dev.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Update(ctx, client, tc.device, tc.change)
			after, rerr := client.Read.GetDevice(ctx, dev.ID)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
				if SettingsState(&after) != SettingsState(&before) {
					t.Errorf("a refused change changed the device: %+v, was %+v", after, before)
				}
				return
			}
			if err != nil || SettingsState(&got) != SettingsState(&after) || !tc.check(after) {
				t.Errorf("Update = %+v, %v; stored %+v", got, err, after)
			}
		})
	}
}

// TestUpdateStalePalette checks that a stored palette that stopped fitting
// the device's model, because its firmware now reports another one, does
// not block a change of its name or policy, and is refused once a change
// names the palette or the profile.
func TestUpdateStalePalette(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	dev, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: testMAC, ApiKeyHash: []byte{1}, FriendlyID: "ABC123", FwModel: "og", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	gray, name, on := "gray-4", "Kitchen", true
	if _, err := Update(ctx, client, "ABC123", Change{Palette: &gray}); err != nil {
		t.Fatal(err)
	}
	// Firmware that reports og_4clr, an og_bwry, which takes no gray.
	if err := client.Write.UpdateDeviceFirmware(ctx, db.UpdateDeviceFirmwareParams{FwModel: "og_4clr", ID: dev.ID}); err != nil {
		t.Fatal(err)
	}
	if got, err := Update(ctx, client, "ABC123", Change{Name: &name, UpdatesEnabled: &on}); err != nil || got.Name != "Kitchen" || got.PaletteOverride != "gray-4" {
		t.Errorf("a rename with a stale palette: %+v, %v", got, err)
	}
	if _, err := Update(ctx, client, "ABC123", Change{Palette: &gray}); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("setting the stale palette again: %v, want invalid", err)
	}
	none := ""
	if got, err := Update(ctx, client, "ABC123", Change{Palette: &none}); err != nil || got.PaletteOverride != "" {
		t.Errorf("clearing it: %+v, %v", got, err)
	}
}

// TestUpdateSeen checks that an update given the settings state a page
// showed goes through, and one given a state from before another change is
// ErrSettingsChanged and changes nothing.
func TestUpdateSeen(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	dev, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: testMAC, ApiKeyHash: []byte{1}, FriendlyID: "ABC123", FwModel: "og", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	seen := SettingsState(&dev)
	name, profile := "Kitchen", "og_bwry"
	if dev, err = Update(ctx, client, "ABC123", Change{Name: &name, Seen: &seen}); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(ctx, client, "ABC123", Change{Profile: &profile, Seen: &seen}); !errors.Is(err, ErrSettingsChanged) || !errors.Is(err, errkind.ErrConflict) {
		t.Errorf("a stale page: %v, want ErrSettingsChanged", err)
	}
	stored, err := client.Read.GetDevice(ctx, dev.ID)
	if err != nil || stored.ProfileOverride != "" || stored.Name != "Kitchen" {
		t.Errorf("after a refused change: %+v, %v", stored, err)
	}
	fresh := SettingsState(&stored)
	if fresh == seen {
		t.Error("a rename did not change the settings state")
	}
	if _, err := Update(ctx, client, "ABC123", Change{Profile: &profile, Seen: &fresh}); err != nil {
		t.Errorf("a fresh page: %v", err)
	}
}

// TestSettingsStateNamesTheDevice checks that two devices with the same
// settings have different states, so a page for a deleted device does not
// match a newer one with its friendly ID.
func TestSettingsStateNamesTheDevice(t *testing.T) {
	t.Parallel()
	a, b := db.Device{ID: 1, FriendlyID: "ABC123"}, db.Device{ID: 2, FriendlyID: "ABC123"}
	if SettingsState(&a) == SettingsState(&b) {
		t.Error("two devices have one state")
	}
}
