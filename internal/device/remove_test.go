package device

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
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
		`INSERT INTO render (id, instance_id, device_id, width, height, palette, time_zone, locale, rendered_at, attempted_at, duration_ms) VALUES (1, 1, 1, 800, 480, 'bw', 'UTC', 'en', 5, 5, 1), (2, 1, 0, 800, 480, 'bw', 'UTC', 'en', 5, 5, 1)`,
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
			dev, err := Delete(ctx, client, name, nil, now)
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
			if _, err := Delete(ctx, client, name, nil, now); !errors.Is(err, ErrNoDevice) {
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
		got, err := Block(ctx, client, tc.device, nil, now)
		if err != nil || got != (db.BlockedMac{Mac: tc.mac, BlockedAt: now.Unix()}) {
			t.Errorf("%s: Block = %+v, %v", tc.name, got, err)
		}
	}
	if got, err := Block(ctx, client, testMAC, nil, now.Add(time.Hour)); err != nil || got.BlockedAt != now.Unix() {
		t.Errorf("a second block: %+v, %v; want the first time kept", got, err)
	}
	if _, err := Block(ctx, client, "ZZZ999", nil, now); !errors.Is(err, ErrNoDevice) {
		t.Errorf("unknown friendly ID: %v", err)
	}
	if n := rowsOf(t, client, 1)["device"]; n != 1 {
		t.Error("a block deleted the device")
	}
	rows, err := ListBlocked(ctx, client.Read)
	if err != nil || len(rows) != 2 || rows[0].Mac != stranger || rows[0].FriendlyID != nil || rows[1].Mac != testMAC || rows[1].FriendlyID == nil || *rows[1].FriendlyID != "ABC123" {
		t.Fatalf("ListBlocked = %+v, %v", rows, err)
	}
	if mac, err := Unblock(ctx, client, "ABC123", nil); err != nil || mac != testMAC {
		t.Errorf("Unblock = %q, %v", mac, err)
	}
	if _, err := Unblock(ctx, client, testMAC, nil); !errors.Is(err, ErrNotBlocked) || !errors.Is(err, errkind.ErrNotFound) {
		t.Errorf("a second unblock: %v, want ErrNotBlocked", err)
	}
	if rows, err = ListBlocked(ctx, client.Read); err != nil || len(rows) != 1 || rows[0].Mac != stranger {
		t.Errorf("after unblock: %+v, %v", rows, err)
	}
}

// TestReadStanding checks the standing of a device by friendly ID and by
// MAC, of a MAC no device has, and of a deleted device's MAC.
func TestReadStanding(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := removeFixture(t)
	const stranger = "D8:3B:DA:00:00:01"
	device1 := Standing{MAC: testMAC, DeviceID: 1, Canary: []int64{1}}
	for _, tc := range []struct {
		device string
		want   Standing
	}{
		{"abc123", device1},
		{strings.ToLower(testMAC), device1},
		{"ABC456", Standing{MAC: "D8:3B:DA:12:34:57", DeviceID: 2}},
		{stranger, Standing{MAC: stranger}},
	} {
		if got, err := ReadStanding(ctx, client.Read, tc.device); err != nil || !got.equal(&tc.want) {
			t.Errorf("%s: %+v, %v; want %+v", tc.device, got, err, tc.want)
		}
	}
	if _, err := ReadStanding(ctx, client.Read, "ZZZ999"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("unknown friendly ID: %v", err)
	}
	now := time.Unix(1_800_000_000, 0)
	if _, err := Delete(ctx, client, testMAC, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := Block(ctx, client, testMAC, nil, now); err != nil {
		t.Fatal(err)
	}
	want := Standing{MAC: testMAC, Blocked: true, Deleted: true}
	if got, err := ReadStanding(ctx, client.Read, testMAC); err != nil || !got.equal(&want) {
		t.Errorf("deleted and blocked: %+v, %v; want %+v", got, err, want)
	}
}

// TestSeenRefused checks that Delete, Block and Unblock given a standing
// that is not the device's now are ErrDeviceChanged and change nothing, and
// that the standing a page read lets them through.
func TestSeenRefused(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	device1 := Standing{MAC: testMAC, DeviceID: 1, Canary: []int64{1}}
	blocked1 := device1
	blocked1.Blocked = true
	type op func(ctx context.Context, client *db.Client, seen *Standing) error
	del := func(ctx context.Context, client *db.Client, seen *Standing) error {
		_, err := Delete(ctx, client, "ABC123", seen, now)
		return err
	}
	block := func(ctx context.Context, client *db.Client, seen *Standing) error {
		_, err := Block(ctx, client, "ABC123", seen, now)
		return err
	}
	unblock := func(ctx context.Context, client *db.Client, seen *Standing) error {
		_, err := Unblock(ctx, client, "ABC123", seen)
		return err
	}
	for _, tc := range []struct {
		name string
		// blocked is whether device 1's MAC is blocked before the change.
		blocked bool
		op      op
		seen    Standing
		ok      bool
	}{
		{"delete as seen", false, del, device1, true},
		{"delete of another device", false, del, Standing{MAC: testMAC, DeviceID: 9, Canary: []int64{1}}, false},
		{"delete of a device blocked since", true, del, device1, false},
		{"delete of a canary not seen as one", false, del, Standing{MAC: testMAC, DeviceID: 1}, false},
		{"delete of another MAC", false, del, Standing{MAC: "D8:3B:DA:12:34:57", DeviceID: 1, Canary: []int64{1}}, false},
		{"block as seen", false, block, device1, true},
		{"block of a device seen unregistered", false, block, Standing{MAC: testMAC}, false},
		{"unblock as seen", true, unblock, blocked1, true},
		{"unblock of a device seen as a tombstone", true, unblock, Standing{MAC: testMAC, Blocked: true, Deleted: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := removeFixture(t)
			if tc.blocked {
				if _, err := Block(t.Context(), client, testMAC, nil, now); err != nil {
					t.Fatal(err)
				}
			}
			before, err := ReadStanding(t.Context(), client.Read, testMAC)
			if err != nil {
				t.Fatal(err)
			}
			err = tc.op(t.Context(), client, &tc.seen)
			if tc.ok {
				if err != nil {
					t.Fatalf("a matching standing was refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrDeviceChanged) || !errors.Is(err, errkind.ErrConflict) {
				t.Fatalf("err = %v, want ErrDeviceChanged", err)
			}
			if after, err := ReadStanding(t.Context(), client.Read, testMAC); err != nil || !after.equal(&before) {
				t.Errorf("a refused change changed the standing from %+v to %+v, %v", before, after, err)
			}
		})
	}
}

// TestSeenReissuedFriendlyID checks that a page for a deleted device
// cannot delete or block a newer device that was given the same friendly
// ID.
func TestSeenReissuedFriendlyID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := removeFixture(t)
	now := time.Unix(1_800_000_000, 0)
	seen, err := ReadStanding(ctx, client.Read, "ABC123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Delete(ctx, client, "ABC123", &seen, now); err != nil {
		t.Fatal(err)
	}
	newer, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: "D8:3B:DA:00:00:03", ApiKeyHash: []byte{3}, FriendlyID: "ABC123", CreatedAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Delete(ctx, client, "ABC123", &seen, now); !errors.Is(err, ErrDeviceChanged) {
		t.Errorf("delete of the newer device: %v, want ErrDeviceChanged", err)
	}
	if _, err := Block(ctx, client, "ABC123", &seen, now); !errors.Is(err, ErrDeviceChanged) {
		t.Errorf("block of the newer device: %v, want ErrDeviceChanged", err)
	}
	if st, err := ReadStanding(ctx, client.Read, "ABC123"); err != nil || st.DeviceID != newer.ID || st.Blocked {
		t.Errorf("the newer device: %+v, %v", st, err)
	}
}

// TestUnblockNotBlockedWithSeen checks that an unblock of a MAC that is not
// blocked is ErrNotBlocked, not ErrDeviceChanged, whatever the page saw.
func TestUnblockNotBlockedWithSeen(t *testing.T) {
	t.Parallel()
	client := removeFixture(t)
	seen := Standing{MAC: testMAC, DeviceID: 1, Blocked: true, Canary: []int64{1}}
	if _, err := Unblock(t.Context(), client, "ABC123", &seen); !errors.Is(err, ErrNotBlocked) || errors.Is(err, ErrDeviceChanged) {
		t.Errorf("err = %v, want ErrNotBlocked", err)
	}
}
