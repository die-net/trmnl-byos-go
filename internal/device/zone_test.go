package device

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // So the zones the tests name load on any host.

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// TestUpdateZoneLocale checks that Update sets a time zone and a locale,
// keeps what a change leaves out, clears each with an empty value, and that
// a refused value changes nothing, even the change's valid fields.
func TestUpdateZoneLocale(t *testing.T) {
	t.Parallel()
	s := func(v string) *string { return &v }
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	dev, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: testMAC, ApiKeyHash: []byte{1}, FriendlyID: "ABC123", Name: "Old", FwModel: "og", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	// stored is the zone and locale of the device now, and the settings state.
	stored := func() (zone, locale, state string) {
		t.Helper()
		d, err := client.Read.GetDevice(ctx, dev.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d.TimeZone, d.Locale, SettingsState(&d)
	}
	for _, tc := range []struct {
		name         string
		change       Change
		want         error
		zone, locale string // The device's after the change, accepted or not.
	}{
		{"a new device has neither", Change{Name: s("Old")}, nil, "", ""},
		{"zone", Change{TimeZone: s("America/Los_Angeles")}, nil, "America/Los_Angeles", ""},
		{"locale, and the zone is kept", Change{Locale: s("pt-BR")}, nil, "America/Los_Angeles", "pt-BR"},
		{"bad zone", Change{Name: s("New"), Locale: s("de"), TimeZone: s("Nowhere/Land")}, errkind.ErrInvalid, "America/Los_Angeles", "pt-BR"},
		{"Local", Change{Name: s("New"), TimeZone: s("Local")}, errkind.ErrInvalid, "America/Los_Angeles", "pt-BR"},
		{"zone with a path", Change{TimeZone: s("../../etc/localtime")}, errkind.ErrInvalid, "America/Los_Angeles", "pt-BR"},
		{"bad locale", Change{Name: s("New"), TimeZone: s("Europe/Paris"), Locale: s("xx")}, errkind.ErrInvalid, "America/Los_Angeles", "pt-BR"},
		{"locale in the wrong case", Change{Locale: s("DE")}, errkind.ErrInvalid, "America/Los_Angeles", "pt-BR"},
		{"both", Change{TimeZone: s("UTC"), Locale: s("zh-HK")}, nil, "UTC", "zh-HK"},
		{"clear the zone", Change{TimeZone: s("")}, nil, "", "zh-HK"},
		{"clear the locale", Change{Locale: s("")}, nil, "", ""},
	} {
		beforeZone, beforeLocale, beforeState := stored()
		_, err := Update(ctx, client, "abc123", tc.change)
		zone, locale, state := stored()
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
			}
			if zone != beforeZone || locale != beforeLocale || state != beforeState {
				t.Errorf("%s: a refused change changed the device: %q %q, was %q %q", tc.name, zone, locale, beforeZone, beforeLocale)
			}
		} else if err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if zone != tc.zone || locale != tc.locale {
			t.Errorf("%s: zone %q locale %q, want %q %q", tc.name, zone, locale, tc.zone, tc.locale)
		}
	}
	if d, err := client.Read.GetDevice(ctx, dev.ID); err != nil || d.Name != "Old" {
		t.Errorf("a refused change renamed the device: %+v, %v", d, err)
	}
}

// TestSettingsStateZoneLocale checks that a change of the time zone or the
// locale changes the settings state, each on its own, so a page that showed
// either is refused after another change of it.
func TestSettingsStateZoneLocale(t *testing.T) {
	t.Parallel()
	base := db.Device{ID: 1, FriendlyID: "ABC123"}
	states := map[string]string{SettingsState(&base): "base"}
	for name, mod := range map[string]func(*db.Device){
		"zone":                 func(d *db.Device) { d.TimeZone = "Asia/Tokyo" },
		"locale":               func(d *db.Device) { d.Locale = "ja" },
		"zone and locale":      func(d *db.Device) { d.TimeZone, d.Locale = "Asia/Tokyo", "ja" },
		"zone moved to locale": func(d *db.Device) { d.Locale = "Asia/Tokyo" },
	} {
		d := base
		mod(&d)
		if prev, dup := states[SettingsState(&d)]; dup {
			t.Errorf("%s has the settings state of %s", name, prev)
		}
		states[SettingsState(&d)] = name
	}
}

// TestUpdateStaleZoneLocale checks that a stored zone that no longer loads,
// or a stored locale that is no longer one of trmnlp's, does not block a
// save of the other settings, nor one that posts it back as it is, as a form
// does, while a change to another bad value is refused.
func TestUpdateStaleZoneLocale(t *testing.T) {
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
	// Stored as a database could hold it, past the checks.
	err = client.Write.UpdateDeviceSettings(ctx, db.UpdateDeviceSettingsParams{TimeZone: "Mars/Olympus", Locale: "tlh", ID: dev.ID})
	if err != nil {
		t.Fatal(err)
	}
	s := func(v string) *string { return &v }
	got, err := Update(ctx, client, "ABC123", Change{Name: s("Kitchen"), TimeZone: s("Mars/Olympus"), Locale: s("tlh")})
	if err != nil || got.Name != "Kitchen" || got.TimeZone != "Mars/Olympus" || got.Locale != "tlh" {
		t.Errorf("a save posting the stale values back: %+v, %v", got, err)
	}
	for _, c := range []Change{{TimeZone: s("Mars/Hellas")}, {Locale: s("vul")}} {
		if _, err := Update(ctx, client, "ABC123", c); !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%+v: %v, want invalid", c, err)
		}
	}
}

// TestZoneOf checks the zone in effect: the device's, the server's when it
// has none, and the server's with one warning when the stored name does not
// load.
func TestZoneOf(t *testing.T) { //nolint:paralleltest // It replaces the process-wide default logger to count warnings, so it cannot run beside another test that does.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	server := time.FixedZone("SRV", 3600)
	for _, tc := range []struct {
		name, stored string
		want         *time.Location
		wantName     string
	}{
		{"override", "Asia/Tokyo", nil, "Asia/Tokyo"},
		{"override again, from the cache", "Asia/Tokyo", nil, "Asia/Tokyo"},
		{"none", "", server, "SRV"},
		{"stored name that does not load", "Gone/Zone", server, "SRV"},
		{"the same name again", "Gone/Zone", server, "SRV"},
		{"Local stored", "Local", server, "SRV"},
	} {
		got := ZoneOf(tc.stored, server)
		if tc.want != nil && got != tc.want {
			t.Errorf("%s: ZoneOf(%q) = %v, want the server's zone", tc.name, tc.stored, got)
		}
		if got.String() != tc.wantName {
			t.Errorf("%s: ZoneOf(%q) = %v, want %s", tc.name, tc.stored, got, tc.wantName)
		}
	}
	// The override is Tokyo's wall clock, not the server's.
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	if h := at.In(ZoneOf("Asia/Tokyo", server)).Hour(); h != 9 {
		t.Errorf("hour in Tokyo = %d, want 9", h)
	}
	if n := strings.Count(logs.String(), "Gone/Zone"); n != 1 {
		t.Errorf("a name that does not load was logged %d times, want 1:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 {
		t.Errorf("%d warnings, want 2 (Gone/Zone and Local):\n%s", n, logs.String())
	}
}

// TestLocaleOf checks the locale in effect: the device's, the server's when
// it has none, and the server's with one warning when the stored code is not
// one of the locales.
func TestLocaleOf(t *testing.T) { //nolint:paralleltest // It replaces the process-wide default logger to count warnings, so it cannot run beside another test that does.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, tc := range []struct{ name, stored, server, want string }{
		{"override", "de", "en", "de"},
		{"override with a region", "pt-BR", "en", "pt-BR"},
		{"none", "", "fr", "fr"},
		{"a code that is no longer a locale", "xx", "fr", "fr"},
		{"the same code again", "xx", "fr", "fr"},
	} {
		if got := LocaleOf(tc.stored, tc.server); got != tc.want {
			t.Errorf("%s: LocaleOf(%q, %q) = %q, want %q", tc.name, tc.stored, tc.server, got, tc.want)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, logs.String())
	}
}
