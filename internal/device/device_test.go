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
