package device

import (
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// removeFixture is a database with device 1 (testMAC, ABC123) and device 2
// (otherMAC, ABC456), each with state, a cursor, a log entry, a playlist
// link and a firmware offer. Device 1 is a rollout's canary and has a render
// of its own, with image dev1; a shared render has image shared. Device 1's
// last image is last1.
func removeFixture(t *testing.T) *db.Client {
	t.Helper()
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	for _, stmt := range []string{
		`INSERT INTO device (id, mac, api_key_hash, friendly_id, created_at) VALUES (1, 'D8:3B:DA:12:34:56', x'01', 'ABC123', 1), (2, 'D8:3B:DA:12:34:57', x'02', 'ABC456', 1)`,
		`INSERT INTO device_state (device_id, last_seen, last_refresh_rate_sent, last_image) VALUES (1, 100, 900, 'last1'), (2, 100, 900, 'last2')`,
		`INSERT INTO device_cursor (device_id, item_id, item_started_at) VALUES (1, 10, 90), (2, 10, 90)`,
		`INSERT INTO device_log (device_id, received_at, device_at, level, message, detail) VALUES (1, 1, 1, 'info', 'a', 'x'), (2, 1, 1, 'info', 'b', 'y')`,
		`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (1, 'r', 's', 900, 1)`,
		`INSERT INTO plugin_instance (id, recipe_id, name, field_values, created_at) VALUES (1, 1, 'i', '{}', 1)`,
		`INSERT INTO playlist (id, name) VALUES (1, 'p')`,
		`INSERT INTO playlist_device (device_id, playlist_id, priority) VALUES (1, 1, 0), (2, 1, 0)`,
		`INSERT INTO render (id, instance_id, device_id, width, height, palette, rendered_at, attempted_at, duration_ms) VALUES (1, 1, 1, 800, 480, 'bw', 5, 5, 1), (2, 1, 0, 800, 480, 'bw', 5, 5, 1)`,
		`INSERT INTO render_image (render_id, layout, image, bytes) VALUES (1, 'full', 'dev1', 3), (2, 'full', 'shared', 3)`,
		`INSERT INTO firmware (id, fw_model, version, sha256, chip, idf_version, bytes, source, uploaded_at) VALUES (1, 'og', '1.9.0', 'abc', 'esp32c3', '5.1', 100, 'upload', 3)`,
		`INSERT INTO firmware_offer (device_id, firmware_id, offers, last_offered_at) VALUES (1, 1, 1, 4), (2, 1, 1, 4)`,
		`INSERT INTO firmware_rollout (firmware_id, canary_device_id, state, started_at) VALUES (1, 1, 'active', 5)`,
	} {
		if _, err := client.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return client
}

// rowsOf counts each table's rows for a device, as device_id or, for a
// rollout, canary_device_id.
func rowsOf(t *testing.T, client *db.Client, id int64) map[string]int {
	t.Helper()
	out := map[string]int{}
	for table, query := range map[string]string{
		"device":           `SELECT COUNT(*) FROM device WHERE id = ?`,
		"device_state":     `SELECT COUNT(*) FROM device_state WHERE device_id = ?`,
		"device_cursor":    `SELECT COUNT(*) FROM device_cursor WHERE device_id = ?`,
		"device_log":       `SELECT COUNT(*) FROM device_log WHERE device_id = ?`,
		"playlist_device":  `SELECT COUNT(*) FROM playlist_device WHERE device_id = ?`,
		"render":           `SELECT COUNT(*) FROM render WHERE device_id = ?`,
		"firmware_offer":   `SELECT COUNT(*) FROM firmware_offer WHERE device_id = ?`,
		"firmware_rollout": `SELECT COUNT(*) FROM firmware_rollout WHERE canary_device_id = ?`,
	} {
		var n int
		if err := client.Reader.QueryRowContext(t.Context(), query, id).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

// TestDelete checks that deleting a device removes every row that names it,
// leaves its MAC as a tombstone, and leaves the other device and the shared
// render alone, so image expiry keeps only the images still in use.
func TestDelete(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"abc123", testMAC} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			client := removeFixture(t)
			now := time.Unix(1_800_000_000, 0)
			dev, err := Delete(ctx, client, name, now)
			if err != nil || dev.ID != 1 || dev.Mac != testMAC {
				t.Fatalf("Delete = %+v, %v", dev, err)
			}
			for table, n := range rowsOf(t, client, 1) {
				if n != 0 {
					t.Errorf("%d %s rows of the deleted device", n, table)
				}
			}
			// The other device has no render or rollout of its own.
			want := map[string]int{"device": 1, "device_state": 1, "device_cursor": 1, "device_log": 1, "playlist_device": 1, "render": 0, "firmware_offer": 1, "firmware_rollout": 0}
			if got := rowsOf(t, client, 2); !maps.Equal(got, want) {
				t.Errorf("rows of the other device = %v, want %v", got, want)
			}
			standing, err := client.Read.GetMACStanding(ctx, testMAC)
			if err != nil || standing != (db.GetMACStandingRow{Deleted: 1}) {
				t.Errorf("standing = %+v, %v", standing, err)
			}
			images, err := client.Read.ListReferencedImages(ctx)
			slices.Sort(images)
			if err != nil || !slices.Equal(images, []string{"last2", "shared"}) {
				t.Errorf("referenced images = %v, %v; want last2 and shared", images, err)
			}
			if _, err := Delete(ctx, client, name, now); !errors.Is(err, ErrNoDevice) {
				t.Errorf("second delete: %v, want ErrNoDevice", err)
			}
		})
	}
}

// TestBlock checks blocking by friendly ID and by a MAC no device has, that
// a block keeps the device row and its first time, and unblocking.
func TestBlock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := removeFixture(t)
	const stranger = "D8:3B:DA:00:00:01"
	now := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		name, device, mac string
	}{
		{"friendly ID", "abc123", testMAC},
		{"unregistered MAC", "d8-3b-da-00-00-01", stranger},
	} {
		got, err := Block(ctx, client, tc.device, now)
		if err != nil || got != (db.BlockedMac{Mac: tc.mac, BlockedAt: now.Unix()}) {
			t.Errorf("%s: Block = %+v, %v", tc.name, got, err)
		}
	}
	if got, err := Block(ctx, client, testMAC, now.Add(time.Hour)); err != nil || got.BlockedAt != now.Unix() {
		t.Errorf("a second block: %+v, %v; want the first time kept", got, err)
	}
	if _, err := Block(ctx, client, "ZZZ999", now); !errors.Is(err, ErrNoDevice) {
		t.Errorf("unknown friendly ID: %v", err)
	}
	if n := rowsOf(t, client, 1)["device"]; n != 1 {
		t.Error("a block deleted the device")
	}
	rows, err := ListBlocked(ctx, client.Read)
	if err != nil || len(rows) != 2 || rows[0].Mac != stranger || rows[0].FriendlyID != nil || rows[1].Mac != testMAC || rows[1].FriendlyID == nil || *rows[1].FriendlyID != "ABC123" {
		t.Fatalf("ListBlocked = %+v, %v", rows, err)
	}
	if mac, err := Unblock(ctx, client, "ABC123"); err != nil || mac != testMAC {
		t.Errorf("Unblock = %q, %v", mac, err)
	}
	if _, err := Unblock(ctx, client, testMAC); !errors.Is(err, ErrNotBlocked) || !errors.Is(err, errkind.ErrNotFound) {
		t.Errorf("a second unblock: %v, want ErrNotBlocked", err)
	}
	if rows, err = ListBlocked(ctx, client.Read); err != nil || len(rows) != 1 || rows[0].Mac != stranger {
		t.Errorf("after unblock: %+v, %v", rows, err)
	}
}
