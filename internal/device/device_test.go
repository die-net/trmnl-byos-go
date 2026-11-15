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

func TestSetName(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_, err = client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: testMAC, ApiKeyHash: []byte{1}, FriendlyID: "ABC123", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetName(ctx, client, "abc123", " Kitchen "); err != nil {
		t.Fatal(err)
	}
	if _, err := SetName(ctx, client, testMAC, strings.Repeat("x", 65)); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("long name: err = %v, want invalid", err)
	}
	if _, err := SetName(ctx, client, "ZZZ999", "x"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("unknown device: err = %v, want ErrNoDevice", err)
	}
	dev, err := client.Read.GetDeviceByMAC(ctx, testMAC)
	if err != nil || dev.Name != "Kitchen" {
		t.Errorf("device %+v, %v; want name Kitchen", dev, err)
	}
	if _, err := SetName(ctx, client, testMAC, ""); err != nil {
		t.Fatal(err)
	}
	if dev, _ = client.Read.GetDeviceByMAC(ctx, testMAC); dev.Name != "" {
		t.Errorf("name %q, want none", dev.Name)
	}
}
