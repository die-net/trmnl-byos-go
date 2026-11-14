package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Client {
	t.Helper()
	client, err := Open(t.Context(), filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestOpenPools checks that the writer runs WAL with synchronous=NORMAL and
// foreign keys on, and that the reader pool refuses writes.
func TestOpenPools(t *testing.T) {
	ctx := t.Context()
	client := openTest(t)

	var mode string
	if err := client.Reader.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("reader journal_mode = %q, %v; want wal", mode, err)
	}
	var sync int
	if err := client.Writer.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil || sync != 1 {
		t.Fatalf("writer synchronous = %d, %v; want 1 (NORMAL)", sync, err)
	}
	var fk int
	if err := client.Writer.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("writer foreign_keys = %d, %v; want 1", fk, err)
	}
	set := SetSettingParams{Key: "k", Value: "v", UpdatedAt: 1}
	if err := client.Read.SetSetting(ctx, set); err == nil {
		t.Fatal("reader accepted a write")
	}
	if err := client.Write.SetSetting(ctx, set); err != nil {
		t.Fatalf("writer: %v", err)
	}
	if v, err := client.Read.GetSetting(ctx, "k"); err != nil || v != "v" {
		t.Fatalf("GetSetting = %q, %v; want v", v, err)
	}
}

// TestReopen checks that opening an existing database applies the schema
// again without error and keeps its rows.
func TestReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "trmnl.db")
	for i := range 2 {
		client, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if i == 0 {
			err = client.Write.SetSetting(ctx, SetSettingParams{Key: "k", Value: "v", UpdatedAt: 1})
		} else {
			_, err = client.Read.GetSetting(ctx, "k")
		}
		if cerr := client.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
}

// TestInTx checks that InTx commits what fn wrote when it returns nil, and
// rolls all of it back when fn fails.
func TestInTx(t *testing.T) {
	ctx := t.Context()
	client := openTest(t)

	insert := func(mac string) func(q *Queries) error {
		return func(q *Queries) error {
			_, err := q.CreateDevice(ctx, CreateDeviceParams{Mac: mac, ApiKeyHash: []byte("key-" + mac), FriendlyID: "id-" + mac, CreatedAt: 1})
			return err
		}
	}
	count := func() int {
		t.Helper()
		devices, err := client.Read.ListDeviceStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(devices)
	}
	boom := errors.New("boom")
	err := client.InTx(ctx, func(q *Queries) error {
		if err := insert("a")(q); err != nil {
			return err
		}
		if err := insert("b")(q); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx = %v, want boom", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d rows after a rollback", n)
	}
	if err := client.InTx(ctx, insert("a")); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("%d rows after a commit, want 1", n)
	}
}

// TestWriterSerializes checks that concurrent InTx calls queue for the one
// writer connection rather than failing with SQLITE_BUSY.
func TestWriterSerializes(t *testing.T) {
	ctx := t.Context()
	client := openTest(t)

	const n = 32
	errs := make(chan error, n)
	for i := range n {
		go func() {
			errs <- client.InTx(ctx, func(q *Queries) error {
				mac := strconv.Itoa(i)
				_, err := q.CreateDevice(ctx, CreateDeviceParams{Mac: mac, ApiKeyHash: []byte("key-" + mac), FriendlyID: "id-" + mac, CreatedAt: 1})
				return err
			})
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	devices, err := client.Read.ListDeviceStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != n {
		t.Fatalf("%d devices, want %d", len(devices), n)
	}
}

// The device table as it first shipped.
const preColumnsDevice = `CREATE TABLE device (
  id INTEGER PRIMARY KEY,
  mac TEXT NOT NULL UNIQUE,
  api_key_hash BLOB NOT NULL UNIQUE,
  friendly_id TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT '',
  fw_model TEXT NOT NULL DEFAULT '',
  fw_version TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
)`

// The device_state table as it first shipped.
const preColumnsState = `CREATE TABLE device_state (
  device_id INTEGER PRIMARY KEY REFERENCES device (id) ON DELETE CASCADE,
  last_seen INTEGER NOT NULL,
  battery_v REAL,
  rssi INTEGER,
  wake_time_ms INTEGER,
  update_source TEXT,
  last_refresh_rate_sent INTEGER NOT NULL,
  last_image TEXT NOT NULL DEFAULT ''
)`

// The render table as it was before data_at. The tables it refers to
// are not made, so the foreign keys are off to insert a row.
const preColumnsRender = `CREATE TABLE render (
  id INTEGER PRIMARY KEY,
  instance_id INTEGER NOT NULL REFERENCES plugin_instance (id) ON DELETE CASCADE,
  device_id INTEGER NOT NULL DEFAULT 0,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  palette TEXT NOT NULL,
  rendered_at INTEGER NOT NULL DEFAULT 0,
  attempted_at INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  log TEXT NOT NULL DEFAULT '',
  UNIQUE (instance_id, device_id, width, height, palette)
)`

// preColumnsSQL builds a database as it was before the columns of
// addedColumns existed, with one row in each of three tables. The tables the
// render table refers to are not made, so the foreign keys are off to insert
// a row.
var preColumnsSQL = []string{
	`PRAGMA foreign_keys = OFF`,
	preColumnsDevice,
	preColumnsState,
	preColumnsRender,
	`INSERT INTO device (mac, api_key_hash, friendly_id, created_at) VALUES ('m', x'00', 'F', 1)`,
	`INSERT INTO device_state (device_id, last_seen, last_refresh_rate_sent, last_image) VALUES (1, 5, 900, 'img')`,
	`INSERT INTO render (instance_id, width, height, palette, rendered_at, attempted_at, duration_ms) VALUES (1, 800, 480, 'bw', 5, 5, 1)`,
}

// preDeviceIDSQL builds a database whose render tables are as they first
// shipped, without device_id, with a render and its image. The tables they
// refer to are not made, so the foreign keys are off to insert rows.
var preDeviceIDSQL = []string{
	`PRAGMA foreign_keys = OFF`,
	`CREATE TABLE render (
  id INTEGER PRIMARY KEY,
  instance_id INTEGER NOT NULL REFERENCES plugin_instance (id) ON DELETE CASCADE,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  palette TEXT NOT NULL,
  rendered_at INTEGER NOT NULL DEFAULT 0,
  attempted_at INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  log TEXT NOT NULL DEFAULT '',
  UNIQUE (instance_id, width, height, palette)
)`,
	`CREATE TABLE render_image (
  render_id INTEGER NOT NULL REFERENCES render (id) ON DELETE CASCADE,
  layout TEXT NOT NULL,
  image TEXT NOT NULL,
  bytes INTEGER NOT NULL,
  PRIMARY KEY (render_id, layout)
)`,
	`INSERT INTO render (instance_id, width, height, palette, rendered_at, attempted_at, duration_ms) VALUES (1, 800, 480, 'bw', 5, 5, 1)`,
	`INSERT INTO render_image (render_id, layout, image, bytes) VALUES (1, 'full', 'img', 3)`,
}

// TestMigrateAddedColumns checks that opening a database made before a
// column was added adds it, keeps the rows, and leaves them usable.
func TestMigrateAddedColumns(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "trmnl.db")
	old, err := sql.Open(driverName(), writerDSN(path, busyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := old.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range preColumnsSQL {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	for i := range 2 { // The second Open finds the columns already there.
		client, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		dev, err := client.Read.GetDeviceByFriendlyID(ctx, "F")
		if err != nil || dev.Mac != "m" || dev.ScreenOverride != "" || dev.ScreenOverrideUntil != 0 ||
			// The new columns default to off and no pin, then keep what was set.
			dev.UpdatesEnabled != int64(i) || dev.FwPin != []string{"", "1.8.1"}[i] {
			t.Fatalf("open %d: device = %+v, %v", i, dev, err)
		}
		_, curErr := client.Read.GetDeviceCursor(ctx, dev.ID)
		// The render keeps its row, and its data_at is 0 until a build sets it,
		// which the first Open's save does. Its failures column starts at 0.
		renders, qerr := client.Read.ListRenderRows(ctx)
		if qerr != nil || len(renders) != 1 || renders[0].DataAt != int64(i)*7 || renders[0].Failures != 0 {
			t.Fatalf("open %d: renders %+v, %v", i, renders, qerr)
		}
		if _, serr := client.Write.SaveRenderSuccess(ctx, SaveRenderSuccessParams{
			InstanceID: 1, Width: 800, Height: 480, Palette: "bw", RenderedAt: 6, AttemptedAt: 6, DataAt: 7,
		}); serr != nil {
			t.Fatalf("open %d: save render: %v", i, serr)
		}
		refs, rerr := client.Read.ListReferencedImages(ctx)
		// A table added after the database was made is created by Open.
		if _, terr := client.Write.CreateToken(ctx, CreateTokenParams{Name: "t" + strconv.Itoa(i), TokenHash: []byte{byte(i)}, CreatedAt: 1}); terr != nil {
			t.Fatalf("open %d: create token: %v", i, terr)
		}
		// The firmware tables, and the device columns a rollout reads.
		fw, ferr := client.Write.CreateFirmware(ctx, CreateFirmwareParams{
			FwModel: "og", Version: "1.8." + strconv.Itoa(i), Sha256: strings.Repeat(strconv.Itoa(i), 64), Chip: "esp32c3", Bytes: 1, Source: "upload", UploadedAt: 1,
		})
		if ferr != nil {
			t.Fatalf("open %d: create firmware: %v", i, ferr)
		}
		if serr := client.Write.SetDeviceFirmwarePolicy(ctx, SetDeviceFirmwarePolicyParams{UpdatesEnabled: 1, FwPin: "1.8.1", ID: dev.ID}); serr != nil {
			t.Fatalf("open %d: set firmware policy: %v", i, serr)
		}
		if serr := client.Write.StartFirmwareRollout(ctx, StartFirmwareRolloutParams{FirmwareID: fw.ID, CanaryDeviceID: dev.ID, StartedAt: 1}); serr != nil {
			t.Fatalf("open %d: start rollout: %v", i, serr)
		}
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
		if rerr != nil || !errors.Is(curErr, sql.ErrNoRows) || len(refs) != 1 || refs[0] != "img" {
			t.Fatalf("open %d: cursor %v; images %v, %v", i, curErr, refs, rerr)
		}
	}
}

// TestMigrateRenderDevice checks that opening a database whose render table
// has no device_id drops its renders, keeps the rest, and leaves a table
// that holds one render per device and removes a deleted device's.
func TestMigrateRenderDevice(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "trmnl.db")
	old, err := sql.Open(driverName(), writerDSN(path, busyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := old.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range preDeviceIDSQL {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	for i := range 2 { // The second Open finds the new shape and keeps its two rows.
		client, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if i == 0 {
			if refs, err := client.Read.ListReferencedImages(ctx); err != nil || len(refs) != 0 {
				t.Fatalf("images after the migration: %v, %v", refs, err)
			}
			checkRenderPerDevice(t, client)
		}
		var n int
		err = client.Read.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM render`).Scan(&n)
		if cerr := client.Close(); err == nil {
			err = cerr
		}
		if err != nil || n != 2 {
			t.Fatalf("open %d: %d renders, %v", i, n, err)
		}
	}
}

// checkRenderPerDevice saves a shared render and two devices' renders of one
// instance, then deletes a device and checks that its render and image go.
func checkRenderPerDevice(t *testing.T, client *Client) {
	t.Helper()
	ctx := t.Context()
	rec, err := client.Write.CreateRecipe(ctx, CreateRecipeParams{Name: "r", Source: "s", RefreshInterval: 900, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := client.Write.CreateInstance(ctx, CreateInstanceParams{RecipeID: rec.ID, Name: "i", FieldValues: "{}", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	devices := make([]int64, 0, 2)
	for _, mac := range []string{"a", "b"} {
		dev, err := client.Write.CreateDevice(ctx, CreateDeviceParams{Mac: mac, ApiKeyHash: []byte(mac), FriendlyID: mac, CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		devices = append(devices, dev.ID)
	}
	for i, device := range append([]int64{0}, devices...) {
		id, err := client.Write.SaveRenderSuccess(ctx, SaveRenderSuccessParams{
			InstanceID: inst.ID, DeviceID: device, Width: 800, Height: 480, Palette: "bw", RenderedAt: 5, AttemptedAt: 5,
		})
		if err != nil {
			t.Fatalf("device %d: %v", device, err)
		}
		err = client.Write.SaveRenderImage(ctx, SaveRenderImageParams{RenderID: id, Layout: "full", Image: "img" + strconv.Itoa(i), Bytes: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.Writer.ExecContext(ctx, `DELETE FROM device WHERE id = ?`, devices[0]); err != nil {
		t.Fatal(err)
	}
	refs, err := client.Read.ListReferencedImages(ctx)
	if err != nil || len(refs) != 2 || refs[0] != "img0" || refs[1] != "img2" {
		t.Fatalf("images after deleting a device: %v, %v", refs, err)
	}
}

// shapeOf is schemaShape, failing the test on an error.
func shapeOf(t *testing.T, db *sql.DB) []string {
	t.Helper()
	shape, err := schemaShape(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	return shape
}

// rawDB opens path without Open, so a test can build an old database.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open(driverName(), writerDSN(path, busyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRowContext(t.Context(), `SELECT user_version FROM pragma_user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestMigrateMatchesSchema checks that a new database (schema.sql) and one
// upgraded from each older shape end with the same tables, columns, indexes
// and triggers, and the same version. The shapes are the schema of commit
// a0122ed, the last release before versions were numbered, and the databases
// that predate the columns of addedColumns and the render table's device_id.
// It fails when schema.sql changes without a step that makes the same change
// to an existing database.
func TestMigrateMatchesSchema(t *testing.T) {
	ctx := t.Context()
	oldSQL, err := os.ReadFile("testdata/schema_a0122ed.sql")
	if err != nil {
		t.Fatal(err)
	}
	fresh := openTest(t)
	want := shapeOf(t, fresh.Writer)
	if len(want) < 20 {
		t.Fatalf("shape has only %d lines, so the comparison proves nothing", len(want))
	}
	if v := userVersion(t, fresh.Writer); v != len(steps) {
		t.Fatalf("fresh version = %d, want %d", v, len(steps))
	}
	for _, tc := range []struct {
		name  string
		build []string
	}{
		{"a0122ed", []string{string(oldSQL)}},
		{"before added columns", preColumnsSQL},
		{"before render device_id", preDeviceIDSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")
			raw := rawDB(t, path)
			for _, stmt := range tc.build {
				if _, err := raw.ExecContext(ctx, stmt); err != nil {
					t.Fatal(err)
				}
			}
			if v := userVersion(t, raw); v != 0 {
				t.Fatalf("old database version = %d, want 0", v)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			upgraded, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.Close()
			got := shapeOf(t, upgraded.Writer)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("upgraded database differs from schema.sql\nupgraded:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if v := userVersion(t, upgraded.Writer); v != len(steps) {
				t.Fatalf("upgraded version = %d, want %d", v, len(steps))
			}
		})
	}
}

// TestMigrateRefusesUnadoptable checks that step 1 refuses a database it
// cannot bring to the baseline: the first commit's device table has api_key
// and no api_key_hash. The error names the column, the version stays 0, and
// the rows are untouched.
func TestMigrateRefusesUnadoptable(t *testing.T) {
	ctx := t.Context()
	firstSQL, err := os.ReadFile("testdata/schema_1371c05.sql")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "first.db")
	raw := rawDB(t, path)
	for _, stmt := range []string{
		string(firstSQL),
		`INSERT INTO device (mac, api_key, friendly_id, created_at) VALUES ('m', 'secret', 'F', 1)`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	before := shapeOf(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(ctx, path)
	if err == nil {
		t.Fatal("open stamped a database that step 1 cannot bring to the baseline")
	}
	if !strings.Contains(err.Error(), "api_key") || !strings.Contains(err.Error(), "device") {
		t.Fatalf("error %q does not name the column that differs", err)
	}
	raw = rawDB(t, path)
	var key string
	if err := raw.QueryRowContext(ctx, `SELECT api_key FROM device WHERE mac = 'm'`).Scan(&key); err != nil || key != "secret" {
		t.Fatalf("device row after the refusal: %q, %v", key, err)
	}
	if v := userVersion(t, raw); v != 0 {
		t.Fatalf("version after the refusal = %d, want 0", v)
	}
	if got := shapeOf(t, raw); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatalf("a refused database changed\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), strings.Join(got, "\n"))
	}
}

// TestBaselineFrozen pins baselineSchema. Step 1 runs it for every database
// made before versions were numbered, so an edit, even one that matches an
// edit to schema.sql, never reaches a database that is already stamped.
func TestBaselineFrozen(t *testing.T) {
	const want = "1eb16908c827b3d7d46bc8ab50fb596689613ef8750810bd0b47aaab101e12ce"
	sum := sha256.Sum256([]byte(baselineSchema))
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("baselineSchema hash = %s, want %s. The baseline must never change. Add a migration step for the schema change instead.", got, want)
	}
}

// TestMigrateVersion checks the version rules: a stamped database is not
// probed or repaired again, and one from a newer server is refused.
func TestMigrateVersion(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "trmnl.db")
	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// Drop a column the baseline adds on demand. A stamped database does not
	// get it back, which shows that step 1 does not run again.
	if _, err := client.Writer.ExecContext(ctx, `ALTER TABLE render DROP COLUMN failures`); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	client, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	err = client.Writer.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('render') WHERE name = 'failures'`).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("failures column count = %d, %v; a stamped database was probed again", n, err)
	}
	if _, err := client.Writer.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(steps)+1)); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open of a newer database = %v, want a refusal", err)
	}
}

// TestMigrateRollsBack checks that a step that fails leaves the database
// at its old version with its old tables.
func TestMigrateRollsBack(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "trmnl.db")
	raw := rawDB(t, path)
	// The baseline creates the missing tables, then cannot add a column to
	// device_state, which here is a view.
	for _, stmt := range []string{
		`CREATE TABLE other (id INTEGER)`,
		`CREATE VIEW device_state AS SELECT 1 AS device_id`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path); err == nil {
		t.Fatal("open succeeded on a database a step cannot upgrade")
	}
	raw = rawDB(t, path)
	var n int
	err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'setting'`).Scan(&n)
	if err != nil || n != 0 || userVersion(t, raw) != 0 {
		t.Fatalf("after a failed step: setting tables %d, version %d, %v", n, userVersion(t, raw), err)
	}
}

// TestMigrateCursors checks step 2: a cursor in device_state moves to
// device_cursor, a device without one gets no row, and a state row that
// only held a cursor, written with a refresh rate of 0 and a made-up
// last_seen, is dropped, so its device is not taken to have woken.
func TestMigrateCursors(t *testing.T) {
	ctx := t.Context()
	oldSQL, err := os.ReadFile("testdata/schema_a0122ed.sql")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "old.db")
	raw := rawDB(t, path)
	stmts := make([]string, 0, 7)
	stmts = append(stmts, string(oldSQL))
	for i, mac := range []string{"a", "b", "c"} {
		n := strconv.Itoa(i + 1)
		stmts = append(stmts, `INSERT INTO device (mac, api_key_hash, friendly_id, created_at) VALUES ('`+mac+`', x'0`+n+`', 'F`+n+`', 1)`)
	}
	stmts = append(stmts,
		// Woke and has a cursor.
		`INSERT INTO device_state (device_id, last_seen, last_refresh_rate_sent, item_id, item_started_at) VALUES (1, 100, 900, 7, 90)`,
		// Woke, has no cursor.
		`INSERT INTO device_state (device_id, last_seen, last_refresh_rate_sent) VALUES (2, 100, 900)`,
		// Never woke. Its row came from SetDeviceCursor.
		`INSERT INTO device_state (device_id, last_seen, last_refresh_rate_sent, item_id, item_started_at) VALUES (3, 100, 0, 8, 95)`,
	)
	for _, stmt := range stmts {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	cur, err := client.Read.GetDeviceCursor(ctx, 1)
	if err != nil || cur.ItemID != 7 || cur.ItemStartedAt != 90 {
		t.Errorf("device 1 cursor %+v, %v", cur, err)
	}
	// The cursor of a device that never woke is kept, only its state row goes.
	cur, err = client.Read.GetDeviceCursor(ctx, 3)
	if err != nil || cur.ItemID != 8 {
		t.Errorf("device 3 cursor %+v, %v", cur, err)
	}
	if _, err := client.Read.GetDeviceCursor(ctx, 2); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("device 2 cursor: %v, want no rows", err)
	}
	var states int
	if err := client.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_state`).Scan(&states); err != nil || states != 2 {
		t.Errorf("%d device_state rows, want 2: %v", states, err)
	}
	if _, err := client.Read.ListDeviceStatus(ctx); err != nil {
		t.Fatal(err)
	}
}
