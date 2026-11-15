package device

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

const testMAC = "D8:3B:DA:12:34:56"

func TestCalibrate(t *testing.T) {
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
