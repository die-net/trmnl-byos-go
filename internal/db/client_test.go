package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestInTxCanceledBeforeCommit checks that a cancel that database/sql has
// already rolled the transaction back for, before fn returns, makes InTx
// return the cancellation, so a caller can tell a shutdown from a failure.
// The writer pool has one connection, and it is free again only after the
// rollback, so fn waiting for it makes the rollback happen first, with no
// timing involved.
func TestInTxCanceledBeforeCommit(t *testing.T) {
	t.Parallel()
	client := openTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := client.InTx(ctx, func(_ *Queries) error {
		cancel()
		conn, err := client.Writer.Conn(t.Context())
		if err != nil {
			return err
		}
		return conn.Close()
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("InTx = %v, want a wrapped context.Canceled", err)
	}
	if !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("InTx = %v, want the original sql.ErrTxDone kept", err)
	}
}

// TestTxDoneAfterCancel checks that a statement inside a transaction that a
// cancel rolled back, which fails with sql.ErrTxDone, makes InTx and ReadTx
// return the cancellation too, and that the same error with a live context
// is not changed.
func TestTxDoneAfterCancel(t *testing.T) {
	t.Parallel()
	client := openTest(t)
	for name, run := range map[string]func(context.Context, func(*Queries) error) error{
		"InTx": client.InTx, "ReadTx": client.ReadTx,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := fmt.Errorf("query: %w", sql.ErrTxDone)
			err := run(ctx, func(*Queries) error {
				cancel()
				return done
			})
			if !errors.Is(err, context.Canceled) || !errors.Is(err, sql.ErrTxDone) {
				t.Errorf("after a cancel: %v, want both context.Canceled and sql.ErrTxDone", err)
			}
			err = run(t.Context(), func(*Queries) error { return done })
			if errors.Is(err, context.Canceled) {
				t.Errorf("with a live context: %v, want no cancellation", err)
			}
		})
	}
}

// TestWriterSerializes checks that concurrent InTx calls queue for the one
// writer connection rather than failing with SQLITE_BUSY.
func TestWriterSerializes(t *testing.T) {
	t.Parallel()
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
// addedColumns existed, with a row in each of its tables and the render's
// parents. Migration refuses a row whose parent is missing, so the render
// has an instance and a recipe. The foreign keys are off to create tables in
// any order.
var preColumnsSQL = []string{
	`PRAGMA foreign_keys = OFF`,
	preColumnsDevice,
	preColumnsState,
	preColumnsRender,
	`CREATE TABLE recipe (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  source TEXT NOT NULL,
  refresh_interval INTEGER NOT NULL,
  installed_at INTEGER NOT NULL
)`,
	`CREATE TABLE plugin_instance (
  id INTEGER PRIMARY KEY,
  recipe_id INTEGER NOT NULL REFERENCES recipe (id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  field_values TEXT NOT NULL DEFAULT '{}',
  secret_values BLOB,
  palette TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
)`,
	`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (1, 'r', 's', 900, 1)`,
	`INSERT INTO plugin_instance (id, recipe_id, name, created_at) VALUES (1, 1, 'i', 1)`,
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
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "trmnl.db")
	old, err := sql.Open(driverName(), writerDSN(path, "rwc", busyTimeout))
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
		if serr := client.Write.UpdateDeviceSettings(ctx, UpdateDeviceSettingsParams{UpdatesEnabled: 1, FwPin: "1.8.1", ProfileOverride: "og_plus", PaletteOverride: "gray-4", ID: dev.ID}); serr != nil {
			t.Fatalf("open %d: update device settings: %v", i, serr)
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
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "trmnl.db")
	old, err := sql.Open(driverName(), writerDSN(path, "rwc", busyTimeout))
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
	raw, err := sql.Open(driverName(), writerDSN(path, "rwc", busyTimeout))
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	const want = "1eb16908c827b3d7d46bc8ab50fb596689613ef8750810bd0b47aaab101e12ce"
	sum := sha256.Sum256([]byte(baselineSchema))
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("baselineSchema hash = %s, want %s. The baseline must never change. Add a migration step for the schema change instead.", got, want)
	}
}

// TestMigrateVersion checks the version rules: a stamped database is not
// probed or repaired again, and one from a newer server is refused.
func TestMigrateVersion(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestMigrateAdminSessions checks step 3: a database at version 2, which
// has no admin_session table, gets it, keeps its other rows, and ends at
// version 3 with the shape of schema.sql.
func TestMigrateAdminSessions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fresh := openTest(t)
	want := shapeOf(t, fresh.Writer)

	path := filepath.Join(t.TempDir(), "v2.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// Make the database look like one from before step 3, which is also from
	// before step 4.
	dropAutoincrement(t, old.Writer)
	for _, stmt := range append(slices.Concat(dropSettingsAt, dropPlaylistPriorityIndex, dropPlaylistSchedules),
		`DROP TABLE admin_session`,
		`INSERT INTO setting (key, value, updated_at) VALUES ('kept', 'yes', 1)`,
		`PRAGMA user_version = 2`,
	) {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 3 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if v, err := client.Read.GetSetting(ctx, "kept"); err != nil || v != "yes" {
		t.Errorf("setting after migration = %q, %v", v, err)
	}
	err = client.Write.CreateAdminSession(ctx, CreateAdminSessionParams{TokenHash: []byte("h"), CreatedAt: 1, LastSeenAt: 1, ExpiresAt: 2})
	if err != nil {
		t.Fatalf("insert into the new table: %v", err)
	}
	// The token hash is unique.
	err = client.Write.CreateAdminSession(ctx, CreateAdminSessionParams{TokenHash: []byte("h"), CreatedAt: 1, LastSeenAt: 1, ExpiresAt: 2})
	if err == nil {
		t.Error("a second session with the same hash was accepted")
	}
}

// dropPlaylistSchedules takes the columns of step 4 out of a database made
// from schema.sql, so that it has the shape from before that step.
var dropPlaylistSchedules = []string{
	`ALTER TABLE playlist DROP COLUMN days`,
	`ALTER TABLE playlist DROP COLUMN window_start`,
	`ALTER TABLE playlist DROP COLUMN window_end`,
	`ALTER TABLE playlist DROP COLUMN starts_at`,
	`ALTER TABLE playlist DROP COLUMN ends_at`,
	`ALTER TABLE playlist_item DROP COLUMN hidden`,
	`ALTER TABLE playlist_item DROP COLUMN days`,
	`ALTER TABLE playlist_item DROP COLUMN window_start`,
	`ALTER TABLE playlist_item DROP COLUMN window_end`,
	`ALTER TABLE playlist_item DROP COLUMN starts_at`,
	`ALTER TABLE playlist_item DROP COLUMN ends_at`,
}

// dropPlaylistPriorityIndex takes the index of step 5 out of a database made
// from schema.sql, so that it has the shape from before that step.
var dropPlaylistPriorityIndex = []string{
	`DROP INDEX playlist_device_priority`,
}

// dropSettingsAt takes the columns of step 6 out of a database made from
// schema.sql, so that it has the shape from before that step.
var dropSettingsAt = []string{
	`ALTER TABLE plugin_instance DROP COLUMN settings_at`,
	`ALTER TABLE plugin_instance DROP COLUMN secret_keys`,
	`ALTER TABLE render DROP COLUMN settings_at`,
}

// TestMigratePlaylistSchedules checks step 4: a database at version 3 gets
// the schedule columns with their defaults, ends with the shape of
// schema.sql, and has each playlist's positions compacted to 0 to n-1 in
// their old order, with positions that are far apart, out of order by ID, and
// that a one-row-at-a-time rewrite would collide on.
func TestMigratePlaylistSchedules(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fresh := openTest(t)
	want := shapeOf(t, fresh.Writer)

	path := filepath.Join(t.TempDir(), "v3.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	dropAutoincrement(t, old.Writer)
	stmts := slices.Concat(dropSettingsAt, dropPlaylistPriorityIndex, dropPlaylistSchedules)
	stmts = append(stmts,
		`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (1, 'r', 's', 900, 1)`,
		`INSERT INTO plugin_instance (id, recipe_id, name, created_at) VALUES (1, 1, 'i', 1)`,
		`INSERT INTO playlist (id, name) VALUES (1, 'a'), (2, 'b')`,
		// Playlist a, in position order: items 12, 10, 11, 13. Playlist b: item 20 alone.
		`INSERT INTO playlist_item (id, playlist_id, position, duration) VALUES (10, 1, 5, 60), (11, 1, 9, 60), (12, 1, 1, 60), (13, 1, 10, 60), (20, 2, 7, 60)`,
		`INSERT INTO playlist_slot (item_id, slot, instance_id) VALUES (10, 0, 1), (11, 0, 1), (12, 0, 1), (13, 0, 1), (20, 0, 1)`,
		`PRAGMA user_version = 3`,
	)
	for _, stmt := range stmts {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, tc := range []struct {
		playlist int64
		want     []int64
	}{
		{1, []int64{12, 10, 11, 13}},
		{2, []int64{20}},
	} {
		ids, err := client.Read.ListPlaylistItemIDs(ctx, tc.playlist)
		if err != nil || fmt.Sprint(ids) != fmt.Sprint(tc.want) {
			t.Errorf("playlist %d items %v, %v; want %v", tc.playlist, ids, err, tc.want)
		}
		for i, id := range tc.want {
			item, err := client.Read.GetPlaylistItem(ctx, id)
			if err != nil || item.Position != int64(i) {
				t.Errorf("item %d position %d, %v; want %d", id, item.Position, err, i)
			}
			if item.Hidden != 0 || item.Days != 127 || item.WindowStart != 0 || item.WindowEnd != 0 || item.StartsAt != 0 || item.EndsAt != 0 {
				t.Errorf("item %d is not the default schedule: %+v", id, item)
			}
		}
	}
	pl, err := client.Read.GetPlaylist(ctx, 1)
	if err != nil || pl.Days != 127 || pl.WindowStart != 0 || pl.WindowEnd != 0 || pl.StartsAt != 0 || pl.EndsAt != 0 {
		t.Errorf("playlist after migration: %+v, %v", pl, err)
	}
}

// TestMigratePlaylistPriorityIndex checks step 5: a database at version 4,
// whose links may have tied or far-apart priorities, gets each device's links
// renumbered 0 to n-1 in their old order (priority, then playlist ID), the
// unique index, and the shape of schema.sql.
func TestMigratePlaylistPriorityIndex(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fresh := openTest(t)
	want := shapeOf(t, fresh.Writer)

	path := filepath.Join(t.TempDir(), "v4.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	dropAutoincrement(t, old.Writer)
	stmts := slices.Concat(dropSettingsAt, dropPlaylistPriorityIndex, []string{
		`INSERT INTO device (id, mac, api_key_hash, friendly_id, created_at) VALUES (1, 'D8:3B:DA:12:34:01', x'01', 'ABC001', 1), (2, 'D8:3B:DA:12:34:02', x'02', 'ABC002', 1), (3, 'D8:3B:DA:12:34:03', x'03', 'ABC003', 1)`,
		`INSERT INTO playlist (id, name) VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd')`,
		// Device 1: all tied at 0, so the playlist ID decides: 1, 2, 3. Device 2:
		// 4 and 2 tied at 5, 3 at 1, and 1 at -2: order 1, 3, 2, 4.
		// Device 3: one link, far from 0.
		`INSERT INTO playlist_device (device_id, playlist_id, priority) VALUES
		  (1, 3, 0), (1, 1, 0), (1, 2, 0),
		  (2, 4, 5), (2, 2, 5), (2, 3, 1), (2, 1, -2),
		  (3, 4, 40)`,
		`PRAGMA user_version = 4`,
	})
	for _, stmt := range stmts {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 5 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, tc := range []struct {
		device int64
		want   []int64
	}{
		{1, []int64{1, 2, 3}},
		{2, []int64{1, 3, 2, 4}},
		{3, []int64{4}},
	} {
		ids, err := client.Read.ListDevicePlaylistIDs(ctx, tc.device)
		if err != nil || fmt.Sprint(ids) != fmt.Sprint(tc.want) {
			t.Errorf("device %d links %v, %v; want %v", tc.device, ids, err, tc.want)
		}
	}
	for _, tc := range []struct{ device, n int64 }{{1, 3}, {2, 4}, {3, 1}} {
		var lo, hi int64
		err := client.Writer.QueryRowContext(ctx, `SELECT MIN(priority), MAX(priority) FROM playlist_device WHERE device_id = ?`, tc.device).Scan(&lo, &hi)
		if err != nil || lo != 0 || hi != tc.n-1 {
			t.Errorf("device %d priorities %d to %d, %v; want 0 to %d", tc.device, lo, hi, err, tc.n-1)
		}
	}
	// A second link at the same priority is refused by the new index.
	_, err = client.Writer.ExecContext(ctx, `INSERT INTO playlist_device (device_id, playlist_id, priority) VALUES (3, 1, 0)`)
	if err == nil {
		t.Error("two links of one device with the same priority were accepted")
	}
}

// TestMigrateSettingsAt checks step 6: a database at version 5 gets
// settings_at on its instances and renders, 0 in every existing row, and
// secret_keys on its instances, with no field marked, which
// leaves the rows otherwise as they were, and ends with the shape of
// schema.sql.
func TestMigrateSettingsAt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fresh := openTest(t)
	want := shapeOf(t, fresh.Writer)

	path := filepath.Join(t.TempDir(), "v5.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	dropAutoincrement(t, old.Writer)
	stmts := slices.Concat(dropSettingsAt, []string{
		`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (1, 'r', 's', 900, 1)`,
		`INSERT INTO plugin_instance (id, recipe_id, name, field_values, created_at) VALUES (1, 1, 'i', '{"a":"b"}', 7)`,
		`INSERT INTO render (instance_id, width, height, palette, rendered_at, attempted_at, duration_ms, data_at) VALUES (1, 800, 480, 'bw', 5, 6, 10, 4)`,
		`PRAGMA user_version = 5`,
	})
	for _, stmt := range stmts {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 6 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	inst, err := client.Read.GetInstance(ctx, 1)
	if err != nil || inst.SettingsAt != 0 || inst.SecretKeys != "[]" || inst.FieldValues != `{"a":"b"}` || inst.CreatedAt != 7 {
		t.Errorf("instance after migration: %+v, %v", inst, err)
	}
	rows, err := client.Read.ListRenderRows(ctx)
	if err != nil || len(rows) != 1 || rows[0].SettingsAt != 0 || rows[0].DataAt != 4 || rows[0].RenderedAt != 5 {
		t.Errorf("render after migration: %+v, %v", rows, err)
	}
}

// autoincrementTables are the tables with a surrogate id, which are
// AUTOINCREMENT, each with an INSERT of one row whose unique values come from
// ?1, an integer, a DELETE of the row with an id, and a query for all the ids. A row that has a parent refers to the parent rows of
// insertParents.
var autoincrementTables = []struct{ table, insert, del, ids string }{
	{"device", `INSERT INTO device (mac, api_key_hash, friendly_id, created_at) VALUES ('m' || ?1, CAST(?1 AS BLOB), 'F' || ?1, 1)`, `DELETE FROM device WHERE id = ?`, `SELECT id FROM device ORDER BY id`},
	{"device_log", `INSERT INTO device_log (device_id, received_at, device_at, level, message, detail) VALUES (9001, ?1, 1, 'info', 'm', 'd')`, `DELETE FROM device_log WHERE id = ?`, `SELECT id FROM device_log ORDER BY id`},
	{"recipe", `INSERT INTO recipe (name, source, refresh_interval, installed_at) VALUES ('r' || ?1, 's', 900, 1)`, `DELETE FROM recipe WHERE id = ?`, `SELECT id FROM recipe ORDER BY id`},
	{"plugin_instance", `INSERT INTO plugin_instance (recipe_id, name, created_at) VALUES (9001, 'i' || ?1, 1)`, `DELETE FROM plugin_instance WHERE id = ?`, `SELECT id FROM plugin_instance ORDER BY id`},
	{"playlist", `INSERT INTO playlist (name) VALUES ('p' || ?1)`, `DELETE FROM playlist WHERE id = ?`, `SELECT id FROM playlist ORDER BY id`},
	{"playlist_item", `INSERT INTO playlist_item (playlist_id, position) VALUES (9001, ?1)`, `DELETE FROM playlist_item WHERE id = ?`, `SELECT id FROM playlist_item ORDER BY id`},
	{"render", `INSERT INTO render (instance_id, width, height, palette, time_zone, locale, attempted_at, duration_ms) VALUES (9001, ?1, 1, 'bw', 'UTC', 'en', 1, 1)`, `DELETE FROM render WHERE id = ?`, `SELECT id FROM render ORDER BY id`},
	{"api_token", `INSERT INTO api_token (name, token_hash, created_at) VALUES ('t' || ?1, CAST(?1 AS BLOB), 1)`, `DELETE FROM api_token WHERE id = ?`, `SELECT id FROM api_token ORDER BY id`},
	{"admin_session", `INSERT INTO admin_session (token_hash, created_at, last_seen_at, expires_at) VALUES (CAST(?1 AS BLOB), 1, 1, 2)`, `DELETE FROM admin_session WHERE id = ?`, `SELECT id FROM admin_session ORDER BY id`},
	{"firmware", `INSERT INTO firmware (fw_model, version, sha256, chip, idf_version, bytes, source, uploaded_at) VALUES ('og', 'v' || ?1, 's' || ?1, 'esp32c3', '5', 1, 'upload', 1)`, `DELETE FROM firmware WHERE id = ?`, `SELECT id FROM firmware ORDER BY id`},
}

// idQueries is the query for all the ids of each table of autoincrementTables.
var idQueries = func() map[string]string {
	m := make(map[string]string)
	for _, tc := range autoincrementTables {
		m[tc.table] = tc.ids
	}
	return m
}()

// insertParents adds the rows, all with ID 9001, that the rows of
// autoincrementTables refer to.
func insertParents(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO device (id, mac, api_key_hash, friendly_id, created_at) VALUES (9001, 'parent', x'9001', 'PARENT', 1)`,
		`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (9001, 'parent', 's', 900, 1)`,
		`INSERT INTO plugin_instance (id, recipe_id, name, created_at) VALUES (9001, 9001, 'parent', 1)`,
		`INSERT INTO playlist (id, name) VALUES (9001, 'parent')`,
	} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// checkNoIDReuse checks that every table of autoincrementTables is
// AUTOINCREMENT, by what the table does: a row inserted after the highest
// row was deleted gets an ID above it, and by what sqlite_master says of it.
func checkNoIDReuse(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := t.Context()
	insertParents(t, db)
	for i, tc := range autoincrementTables {
		var tableSQL string
		if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, tc.table).Scan(&tableSQL); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(tableSQL, "AUTOINCREMENT") {
			t.Errorf("%s is not AUTOINCREMENT: %s", tc.table, tableSQL)
		}
		n := 7000 + i*10
		ids := make([]int64, 0, 3)
		for j := range 3 {
			res, err := db.ExecContext(ctx, tc.insert, n+j)
			if err != nil {
				t.Fatalf("%s: insert %d: %v", tc.table, j, err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		// Delete the highest row, then insert again.
		if _, err := db.ExecContext(ctx, tc.del, ids[2]); err != nil {
			t.Fatalf("%s: delete: %v", tc.table, err)
		}
		res, err := db.ExecContext(ctx, tc.insert, n+3)
		if err != nil {
			t.Fatalf("%s: insert after delete: %v", tc.table, err)
		}
		if id, err := res.LastInsertId(); err != nil || id <= ids[2] {
			t.Errorf("%s: a new row got ID %d after ID %d was deleted, %v", tc.table, id, ids[2], err)
		}
	}
}

// tableNamePrefix finds the start of a table's CREATE TABLE statement as
// SQLite stores it, whether it was made from schema.sql or renamed.
var tableNamePrefix = regexp.MustCompile(`^CREATE TABLE (IF NOT EXISTS )?("?)(\w+)("?)`)

// dropAutoincrement rebuilds every table of autoincrementTables without
// AUTOINCREMENT, as it was before step 7, in a database made from
// schema.sql that holds no rows yet. It reads each table's SQL, because a
// test may do that, and uses the same rebuild as step 7. It takes out the
// tables and columns of the later steps first, with dropRecipeRevision,
// dropRenderedSettingsAt, dropButtons, dropSleep, dropZoneLocale,
// dropProfileOverride and dropBlocklist. A migration test for an older version calls it first.
func dropAutoincrement(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := t.Context()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(stmt string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for _, stmt := range slices.Concat(dropRenderAttempt, dropRecipeRevision, dropRenderedSettingsAt, dropButtons, dropSleep, dropZoneLocale, dropProfileOverride, dropBlocklist) {
		exec(stmt)
	}
	exec(`PRAGMA foreign_keys = OFF`)
	exec(`DROP TRIGGER render_device_deleted`)
	for _, tc := range autoincrementTables {
		var tableSQL string
		if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, tc.table).Scan(&tableSQL); err != nil {
			t.Fatal(err)
		}
		if !tableNamePrefix.MatchString(tableSQL) {
			t.Fatalf("unexpected SQL for %s: %s", tc.table, tableSQL)
		}
		old := tableNamePrefix.ReplaceAllString(tableSQL, "CREATE TABLE "+tc.table+"_old")
		old = strings.Replace(old, " AUTOINCREMENT", "", 1)
		exec(old)
		exec(`INSERT INTO ` + tc.table + `_old SELECT * FROM ` + tc.table)
		exec(`DROP TABLE ` + tc.table)
		exec(`ALTER TABLE ` + tc.table + `_old RENAME TO ` + tc.table)
	}
	exec(`CREATE INDEX device_log_device ON device_log (device_id, id)`)
	exec(`CREATE TRIGGER render_device_deleted AFTER DELETE ON device
BEGIN
  DELETE FROM render WHERE device_id = OLD.id;
END`)
	exec(`PRAGMA foreign_keys = ON`)
	exec(`PRAGMA user_version = 6`)
}

// TestAutoincrementFresh checks that a new database never hands out an ID
// twice, in every table with a surrogate ID.
func TestAutoincrementFresh(t *testing.T) {
	t.Parallel()
	checkNoIDReuse(t, openTest(t).Writer)
}

// TestMigrateAutoincrement checks step 7: a database at version 6, with rows
// in the tables it rebuilds and in their children, keeps every row and ID,
// its foreign keys, its cascades and its trigger, ends with the shape of
// schema.sql, and no longer reuses an ID.
func TestMigrateAutoincrement(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fresh := openTest(t)
	want := shapeOf(t, fresh.Writer)

	path := filepath.Join(t.TempDir(), "v6.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	dropAutoincrement(t, old.Writer)
	for _, stmt := range []string{
		`INSERT INTO device (id, mac, api_key_hash, friendly_id, created_at) VALUES (1, 'D8:3B:DA:12:34:01', x'01', 'ABC001', 1), (5, 'D8:3B:DA:12:34:05', x'05', 'ABC005', 1)`,
		`INSERT INTO device_state (device_id, last_seen, last_refresh_rate_sent) VALUES (1, 100, 900), (5, 100, 900)`,
		`INSERT INTO device_cursor (device_id, item_id, item_started_at) VALUES (1, 10, 90)`,
		`INSERT INTO device_log (id, device_id, received_at, device_at, level, message, detail) VALUES (2, 1, 1, 1, 'info', 'a', 'x'), (6, 5, 2, 2, 'warn', 'b', 'y')`,
		`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (1, 'r', 's', 900, 1), (4, 'r4', 's4', 60, 2)`,
		`INSERT INTO plugin_instance (id, recipe_id, name, field_values, secret_values, palette, created_at, settings_at, secret_keys) VALUES (3, 1, 'i', '{"a":"b"}', x'0102', 'bw', 7, 8, '["k"]'), (8, 4, 'j', '{}', NULL, '', 9, 0, '[]')`,
		`INSERT INTO instance_data (instance_id, data, updated_at) VALUES (3, '{"d":1}', 5)`,
		`INSERT INTO playlist (id, name, days) VALUES (2, 'a', 62), (7, 'b', 127)`,
		`INSERT INTO playlist_device (device_id, playlist_id, priority) VALUES (1, 2, 0), (1, 7, 1), (5, 7, 0)`,
		`INSERT INTO playlist_item (id, playlist_id, position, duration, hidden) VALUES (10, 2, 0, 60, 0), (12, 2, 1, 30, 1), (20, 7, 0, 60, 0)`,
		`INSERT INTO playlist_slot (item_id, slot, instance_id) VALUES (10, 0, 3), (12, 0, 8), (20, 0, 3)`,
		// Render 2 is built for device 5 and renders 1 and 3 for every device.
		`INSERT INTO render (id, instance_id, device_id, width, height, palette, rendered_at, attempted_at, duration_ms, settings_at) VALUES
		  (1, 3, 0, 800, 480, 'bw', 5, 6, 10, 8), (2, 3, 5, 800, 480, 'bw', 5, 6, 10, 8), (3, 8, 0, 800, 480, 'bw', 5, 6, 10, 0)`,
		`INSERT INTO render_image (render_id, layout, image, bytes) VALUES (1, 'full', 'img1', 3), (2, 'full', 'img2', 3), (3, 'full', 'img3', 3)`,
		`INSERT INTO api_token (id, name, token_hash, created_at, last_used_at) VALUES (3, 'tok', x'aa', 1, 2)`,
		`INSERT INTO admin_session (id, token_hash, created_at, last_seen_at, expires_at) VALUES (4, x'bb', 1, 2, 3)`,
		`INSERT INTO firmware (id, fw_model, version, sha256, chip, idf_version, bytes, source, uploaded_at) VALUES (9, 'og', '1.9.0', 'abc', 'esp32c3', '5.1', 100, 'upload', 3)`,
		`INSERT INTO firmware_offer (device_id, firmware_id, offers, last_offered_at) VALUES (1, 9, 2, 4)`,
		`INSERT INTO firmware_rollout (firmware_id, canary_device_id, state, started_at) VALUES (9, 1, 'active', 5)`,
	} {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if v := userVersion(t, old.Writer); v != 6 {
		t.Fatalf("fixture version = %d, want 6", v)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	db := client.Writer
	if v := userVersion(t, db); v != len(steps) || v < 7 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, db); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Every row is there, with its ID.
	for _, tc := range []struct {
		table string
		ids   []int64
	}{
		{"device", []int64{1, 5}},
		{"device_log", []int64{2, 6}},
		{"recipe", []int64{1, 4}},
		{"plugin_instance", []int64{3, 8}},
		{"playlist", []int64{2, 7}},
		{"playlist_item", []int64{10, 12, 20}},
		{"render", []int64{1, 2, 3}},
		{"api_token", []int64{3}},
		{"admin_session", []int64{4}},
		{"firmware", []int64{9}},
	} {
		rows, err := db.QueryContext(ctx, idQueries[tc.table])
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, tc.ids) {
			t.Errorf("%s IDs %v, want %v", tc.table, got, tc.ids)
		}
	}
	for table, want := range map[string]int{
		"device_state": 2, "device_cursor": 1, "instance_data": 1, "playlist_device": 3, "playlist_slot": 3,
		"render_image": 3, "firmware_offer": 1, "firmware_rollout": 1,
	} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil || n != want {
			t.Errorf("%s has %d rows, want %d, %v", table, n, want, err)
		}
	}
	// Columns came across, including ones whose order differs from schema.sql.
	inst, err := client.Read.GetInstance(ctx, 3)
	if err != nil || inst.RecipeID != 1 || inst.FieldValues != `{"a":"b"}` || !slices.Equal(inst.SecretValues, []byte{1, 2}) ||
		inst.Palette != "bw" || inst.CreatedAt != 7 || inst.SettingsAt != 8 || inst.SecretKeys != `["k"]` {
		t.Errorf("instance 3 after migration: %+v, %v", inst, err)
	}
	pl, err := client.Read.GetPlaylist(ctx, 2)
	if err != nil || pl.Name != "a" || pl.Days != 62 {
		t.Errorf("playlist 2 after migration: %+v, %v", pl, err)
	}

	// Foreign keys are on and nothing is orphaned.
	var fk, orphans int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v", fk, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&orphans); err != nil || orphans != 0 {
		t.Errorf("foreign_key_check found %d rows, %v", orphans, err)
	}

	// A highest ID that is deleted is not handed out again.
	for _, tc := range []struct {
		table, del, insert string
		high               int64
	}{
		{"playlist", `DELETE FROM playlist WHERE id = 7`, `INSERT INTO playlist (name) VALUES ('c')`, 7},
		{"device", `DELETE FROM device WHERE id = 5`, `INSERT INTO device (mac, api_key_hash, friendly_id, created_at) VALUES ('m', x'07', 'F', 1)`, 5},
	} {
		if _, err := db.ExecContext(ctx, tc.del); err != nil {
			t.Fatal(err)
		}
		if tc.table == "device" {
			// The trigger removed the device's render and, by cascade, its image.
			var renders, images int
			err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM render WHERE device_id = 5), (SELECT COUNT(*) FROM render_image WHERE render_id = 2)`).Scan(&renders, &images)
			if err != nil || renders != 0 || images != 0 {
				t.Errorf("after deleting device 5: %d renders, %d images, %v; the trigger did not run", renders, images, err)
			}
		}
		res, err := db.ExecContext(ctx, tc.insert)
		if err != nil {
			t.Fatal(err)
		}
		if id, err := res.LastInsertId(); err != nil || id != tc.high+1 {
			t.Errorf("%s: new row got ID %d after ID %d was deleted, want %d, %v", tc.table, id, tc.high, tc.high+1, err)
		}
	}

	// A cascade still works: deleting device 1 takes its children.
	if _, err := db.ExecContext(ctx, `DELETE FROM device WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM device_state WHERE device_id = 1`,
		`SELECT COUNT(*) FROM device_cursor WHERE device_id = 1`,
		`SELECT COUNT(*) FROM device_log WHERE device_id = 1`,
		`SELECT COUNT(*) FROM playlist_device WHERE device_id = 1`,
		`SELECT COUNT(*) FROM firmware_offer WHERE device_id = 1`,
		`SELECT COUNT(*) FROM firmware_rollout WHERE canary_device_id = 1`,
	} {
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil || n != 0 {
			t.Errorf("after deleting device 1, %q = %d, %v; want 0", q, n, err)
		}
	}
	// So does deleting a recipe, down to the render images.
	if _, err := db.ExecContext(ctx, `DELETE FROM recipe WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	var left int
	err = db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM plugin_instance WHERE id = 3) + (SELECT COUNT(*) FROM playlist_slot WHERE instance_id = 3) + (SELECT COUNT(*) FROM instance_data) + (SELECT COUNT(*) FROM render WHERE instance_id = 3) + (SELECT COUNT(*) FROM render_image WHERE render_id = 1)`).Scan(&left)
	if err != nil || left != 0 {
		t.Errorf("after deleting recipe 1, %d dependent rows are left, %v", left, err)
	}

	checkNoIDReuse(t, db)
}

// TestMigrateForeignKeys checks that migrate leaves foreign keys on, whether
// it succeeds, fails, or refuses a database whose row has no parent, and that
// the refusal rolls back.
func TestMigrateForeignKeys(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, tc := range []struct {
		name    string
		prepare []string
		want    string // A part of the error, or empty for success.
		version int
		v6      bool // Whether the database is rebuilt as version 6 first.
	}{
		{"succeeds", nil, "", len(steps), false},
		{"newer database", []string{`PRAGMA user_version = 99`}, "newer", 99, false},
		{"orphan row", []string{`PRAGMA foreign_keys = OFF`, `INSERT INTO device_log (device_id, received_at, device_at, level, message, detail) VALUES (42, 1, 1, 'i', 'm', 'd')`, `PRAGMA user_version = 6`}, "foreign key", 6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "trmnl.db")
			seed, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.v6 {
				dropAutoincrement(t, seed.Writer)
			}
			conn, err := seed.Writer.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range tc.prepare {
				if _, err := conn.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			if err := errors.Join(conn.Close(), seed.Close()); err != nil {
				t.Fatal(err)
			}

			writer, err := openPool(ctx, path, writerDSN(path, "rwc", busyTimeout), 1)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			client := &Client{Writer: writer, Write: New(writer)}
			err = client.migrate(ctx)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("migrate = %v, want an error with %q", err, tc.want)
			}
			if v := userVersion(t, writer); v != tc.version {
				t.Errorf("version = %d, want %d", v, tc.version)
			}
			var fk int
			if err := writer.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
				t.Errorf("foreign_keys = %d after migrate, %v; want 1", fk, err)
			}
		})
	}
}

// TestReadTx checks that every read in a ReadTx sees one snapshot: a commit
// made through Write after the first read is not seen by the second, though
// it is by a read outside. It runs under whichever driver the build uses.
func TestReadTx(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := openTest(t)
	set := func(v string) {
		t.Helper()
		_, err := client.Writer.ExecContext(ctx, `INSERT INTO setting (key, value, updated_at) VALUES ('k', ?, 1) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, v)
		if err != nil {
			t.Fatal(err)
		}
	}
	set("old")
	err := client.ReadTx(ctx, func(q *Queries) error {
		if v, err := q.GetSetting(ctx, "k"); err != nil || v != "old" {
			t.Errorf("first read = %q, %v", v, err)
		}
		set("new")
		if v, err := q.GetSetting(ctx, "k"); err != nil || v != "old" {
			t.Errorf("second read in the transaction = %q, %v; want the old value", v, err)
		}
		if v, err := client.Read.GetSetting(ctx, "k"); err != nil || v != "new" {
			t.Errorf("read outside the transaction = %q, %v; want the new value", v, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// An error from fn is returned as it is, and the transaction is released.
	want := errors.New("stop")
	if err := client.ReadTx(ctx, func(*Queries) error { return want }); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if v, err := client.Read.GetSetting(ctx, "k"); err != nil || v != "new" {
		t.Errorf("after the transaction = %q, %v", v, err)
	}
}

// dropBlocklist takes the tables of step 8 out of a database made from
// schema.sql, so that it has the shape from before that step.
var dropBlocklist = []string{
	`DROP TABLE device_tombstone`,
	`DROP TABLE blocked_mac`,
}

// TestMigrateBlocklist checks step 8: a database at version 7 gets the
// device_tombstone and blocked_mac tables, keeps its devices, and ends with
// the shape of schema.sql. Each table holds a MAC once.
func TestMigrateBlocklist(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	want := shapeOf(t, openTest(t).Writer)

	path := filepath.Join(t.TempDir(), "v7.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range slices.Concat(dropRenderAttempt, dropRecipeRevision, dropRenderedSettingsAt, dropButtons, dropSleep, dropZoneLocale, dropProfileOverride, dropBlocklist, []string{
		`INSERT INTO device (id, mac, api_key_hash, friendly_id, created_at) VALUES (3, 'D8:3B:DA:12:34:03', x'03', 'ABC003', 1)`,
		`PRAGMA user_version = 7`,
	}) {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 8 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if dev, err := client.Read.GetDevice(ctx, 3); err != nil || dev.FriendlyID != "ABC003" {
		t.Errorf("device after migration: %+v, %v", dev, err)
	}
	const mac = "D8:3B:DA:12:34:09"
	if _, err := client.Write.BlockMAC(ctx, BlockMACParams{Mac: mac, BlockedAt: 5}); err != nil {
		t.Fatal(err)
	}
	if err := client.Write.AddDeviceTombstone(ctx, AddDeviceTombstoneParams{Mac: mac, DeletedAt: 6}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO blocked_mac (mac, blocked_at) VALUES (?, 7)`,
		`INSERT INTO device_tombstone (mac, deleted_at) VALUES (?, 7)`,
	} {
		if _, err := client.Writer.ExecContext(ctx, stmt, mac); err == nil {
			t.Errorf("a MAC was stored twice: %s", stmt)
		}
	}
	got, err := client.Read.GetMACStanding(ctx, mac)
	if err != nil || got != (GetMACStandingRow{Blocked: 1, Deleted: 1}) {
		t.Errorf("standing = %+v, %v", got, err)
	}
}

// dropProfileOverride takes the columns of step 9 out of a database made
// from schema.sql, so that it has the shape from before that step.
var dropProfileOverride = []string{
	`ALTER TABLE device DROP COLUMN profile_override`,
	`ALTER TABLE device DROP COLUMN palette_override`,
}

// TestMigrateProfileOverride checks step 9: a database at version 8 gets the
// device's override columns, empty, keeps its devices, and ends with the
// shape of schema.sql.
func TestMigrateProfileOverride(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	want := shapeOf(t, openTest(t).Writer)

	path := filepath.Join(t.TempDir(), "v8.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range slices.Concat(dropRenderAttempt, dropRecipeRevision, dropRenderedSettingsAt, dropButtons, dropSleep, dropZoneLocale, dropProfileOverride, []string{
		`INSERT INTO device (id, mac, api_key_hash, friendly_id, fw_model, created_at) VALUES (3, 'D8:3B:DA:12:34:03', x'03', 'ABC003', 'og', 1)`,
		`PRAGMA user_version = 8`,
	}) {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 9 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	dev, err := client.Read.GetDevice(ctx, 3)
	if err != nil || dev.FwModel != "og" || dev.ProfileOverride != "" || dev.PaletteOverride != "" {
		t.Errorf("device after migration: %+v, %v", dev, err)
	}
}

// dropZoneLocale takes the columns of step 10 out of a database made from
// schema.sql, so that it has the shape from before that step. SQLite cannot
// drop a column that is in a UNIQUE constraint, so render is made again as it
// was, as step 10 makes it, and its rows keep their other columns.
var dropZoneLocale = []string{
	`PRAGMA foreign_keys = OFF`,
	`DROP TRIGGER render_device_deleted`,
	`CREATE TABLE render_old (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
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
  data_at INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,
  settings_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE (instance_id, device_id, width, height, palette)
)`,
	`INSERT INTO render_old (id, instance_id, device_id, width, height, palette, rendered_at, attempted_at, duration_ms, error, log, data_at, failures, settings_at)
  SELECT id, instance_id, device_id, width, height, palette, rendered_at, attempted_at, duration_ms, error, log, data_at, failures, settings_at FROM render`,
	`DROP TABLE render`,
	`ALTER TABLE render_old RENAME TO render`,
	`CREATE TRIGGER render_device_deleted AFTER DELETE ON device
BEGIN
  DELETE FROM render WHERE device_id = OLD.id;
END`,
	`PRAGMA foreign_keys = ON`,
	`ALTER TABLE device DROP COLUMN time_zone`,
	`ALTER TABLE device DROP COLUMN locale`,
}

// TestMigrateZoneLocaleKeepsSequence checks that step 10 keeps render's
// AUTOINCREMENT counter, so a render made after it gets an ID above every
// one before it, deleted ones included: with rows left below a deleted
// higher ID, and with every row deleted.
func TestMigrateZoneLocaleKeepsSequence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// keep is how many of renders 1 to 9 are left after 9 are made.
		keep int
	}{
		{"rows left below a deleted ID", 3},
		{"every row deleted", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "v9.db")
			old, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range slices.Concat(dropRenderAttempt, dropRecipeRevision, dropRenderedSettingsAt, dropButtons, dropSleep, dropZoneLocale, []string{
				`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (1, 'r', 's', 900, 1)`,
				`INSERT INTO plugin_instance (id, recipe_id, name, created_at) VALUES (1, 1, 'i', 1)`,
				`PRAGMA user_version = 9`,
			}) {
				if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			for i := range 9 {
				_, err := old.Writer.ExecContext(ctx, `INSERT INTO render (instance_id, width, height, palette, attempted_at, duration_ms) VALUES (1, ?, 480, 'bw', 1, 1)`, 100+i)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := old.Writer.ExecContext(ctx, `DELETE FROM render WHERE id > ?`, tc.keep); err != nil {
				t.Fatal(err)
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}

			client, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var id int64
			err = client.Writer.QueryRowContext(ctx, `INSERT INTO render (instance_id, width, height, palette, time_zone, locale, attempted_at, duration_ms) VALUES (1, 1, 1, 'bw', 'UTC', 'en', 1, 1) RETURNING id`).Scan(&id)
			if err != nil || id <= 9 {
				t.Errorf("new render ID = %d, %v; want above 9, the highest before the migration", id, err)
			}
		})
	}
}

// TestMigrateZoneLocale checks step 10: a database at version 9 keeps every
// render row's other columns and its images, gives the rows an empty time
// zone and locale, takes two rows that differ only by zone or locale, still
// removes a deleted device's renders, gives devices an empty zone and
// locale, and ends with the shape of schema.sql.
func TestMigrateZoneLocale(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	want := shapeOf(t, openTest(t).Writer)

	path := filepath.Join(t.TempDir(), "v9.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range slices.Concat(dropRenderAttempt, dropRecipeRevision, dropRenderedSettingsAt, dropButtons, dropSleep, dropZoneLocale, []string{
		`INSERT INTO device (id, mac, api_key_hash, friendly_id, created_at) VALUES (3, 'D8:3B:DA:12:34:03', x'03', 'ABC003', 1)`,
		`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (1, 'r', 's', 900, 1)`,
		`INSERT INTO plugin_instance (id, recipe_id, name, created_at) VALUES (1, 1, 'i', 1)`,
		// Render 4 is the shared one, 5 is device 3's, and 6 has an error.
		`INSERT INTO render (id, instance_id, device_id, width, height, palette, rendered_at, attempted_at, duration_ms, error, log, data_at, failures, settings_at) VALUES
		  (4, 1, 0, 800, 480, 'bw', 50, 60, 700, '', 'log4', 40, 0, 30),
		  (5, 1, 3, 800, 480, 'bw', 51, 61, 701, '', 'log5', 41, 0, 31),
		  (6, 1, 0, 480, 800, 'gray-4', 0, 62, 702, 'boom', 'log6', 42, 3, 32)`,
		`INSERT INTO render_image (render_id, layout, image, bytes) VALUES (4, 'full', 'img4', 4), (4, 'half', 'img4h', 5), (5, 'full', 'img5', 6)`,
		`PRAGMA user_version = 9`,
	}) {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 10 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Every row has its ID and other columns, and an empty zone and locale.
	rows, err := client.Read.ListRenderRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantRows := []ListRenderRowsRow{
		{ID: 4, InstanceID: 1, Width: 800, Height: 480, Palette: "bw", RenderedAt: 50, AttemptedAt: 60, DurationMs: 700, DataAt: 40, SettingsAt: 30},
		{ID: 5, InstanceID: 1, DeviceID: 3, Width: 800, Height: 480, Palette: "bw", RenderedAt: 51, AttemptedAt: 61, DurationMs: 701, DataAt: 41, SettingsAt: 31},
		{ID: 6, InstanceID: 1, Width: 480, Height: 800, Palette: "gray-4", AttemptedAt: 62, DurationMs: 702, Error: "boom", DataAt: 42, Failures: 3, SettingsAt: 32},
	}
	if !slices.Equal(rows, wantRows) {
		t.Errorf("renders after migration:\n%+v\nwant\n%+v", rows, wantRows)
	}
	if st, err := client.Read.GetRenderStatus(ctx, 5); err != nil || st.Log != "log5" {
		t.Errorf("render 5's log: %+v, %v", st, err)
	}
	// Its images are kept, by the render they belong to.
	var images []string
	img, err := client.Writer.QueryContext(ctx, `SELECT render_id || ':' || layout || ':' || image || ':' || bytes FROM render_image ORDER BY render_id, layout`)
	if err != nil {
		t.Fatal(err)
	}
	for img.Next() {
		var row string
		if err := img.Scan(&row); err != nil {
			t.Fatal(err)
		}
		images = append(images, row)
	}
	if err := errors.Join(img.Err(), img.Close()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"4:full:img4:4", "4:half:img4h:5", "5:full:img5:6"}; !slices.Equal(images, want) {
		t.Errorf("render images = %v, want %v", images, want)
	}
	if dev, err := client.Read.GetDevice(ctx, 3); err != nil || dev.TimeZone != "" || dev.Locale != "" {
		t.Errorf("device after migration: %+v, %v", dev, err)
	}

	// Two rows that differ only by zone or locale are two targets, and the
	// same target is one row.
	save := func(zone, locale string) int64 {
		t.Helper()
		id, err := client.Write.SaveRenderSuccess(ctx, SaveRenderSuccessParams{
			InstanceID: 1, DeviceID: 3, Width: 800, Height: 480, Palette: "bw", TimeZone: zone, Locale: locale, RenderedAt: 70, AttemptedAt: 70,
		})
		if err != nil {
			t.Fatalf("save %q %q: %v", zone, locale, err)
		}
		return id
	}
	a, b, c := save("UTC", "en"), save("America/Los_Angeles", "en"), save("UTC", "de")
	if a == b || a == c || b == c || save("UTC", "en") != a {
		t.Errorf("render IDs %d %d %d: want three targets, and a repeat of the first to be the first", a, b, c)
	}
	if _, err := client.Writer.ExecContext(ctx, `INSERT INTO render (instance_id, device_id, width, height, palette, time_zone, locale, attempted_at, duration_ms) VALUES (1, 3, 800, 480, 'bw', 'UTC', 'en', 1, 1)`); err == nil {
		t.Error("two renders of one target were stored")
	}

	// The trigger removes a deleted device's renders, with their images.
	if err := client.Write.SaveRenderImage(ctx, SaveRenderImageParams{RenderID: b, Layout: "full", Image: "imgb", Bytes: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Writer.ExecContext(ctx, `DELETE FROM device WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	var left, leftImages int
	err = client.Writer.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM render WHERE device_id = 3), (SELECT COUNT(*) FROM render_image WHERE render_id IN (5, ?))`, b).Scan(&left, &leftImages)
	if err != nil || left != 0 || leftImages != 0 {
		t.Errorf("after deleting the device: %d renders, %d images, %v", left, leftImages, err)
	}
	if refs, err := client.Read.ListReferencedImages(ctx); err != nil || !slices.Equal(refs, []string{"img4", "img4h"}) {
		t.Errorf("referenced images after deleting the device: %v, %v", refs, err)
	}
}

// dropSleep takes the columns of step 11 out of a database made from
// schema.sql, so that it has the shape from before that step.
var dropSleep = []string{
	`ALTER TABLE device DROP COLUMN sleep_start`,
	`ALTER TABLE device DROP COLUMN sleep_end`,
}

// TestMigrateSleepWindow checks step 11: a database at version 10 gets the
// device's sleep columns, -1 for no window, keeps its devices, and ends with
// the shape of schema.sql.
func TestMigrateSleepWindow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	want := shapeOf(t, openTest(t).Writer)

	path := filepath.Join(t.TempDir(), "v10.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range slices.Concat(dropRenderAttempt, dropRecipeRevision, dropRenderedSettingsAt, dropButtons, dropSleep, []string{
		`INSERT INTO device (id, mac, api_key_hash, friendly_id, fw_model, created_at) VALUES (3, 'D8:3B:DA:12:34:03', x'03', 'ABC003', 'og', 1)`,
		`PRAGMA user_version = 10`,
	}) {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 11 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	dev, err := client.Read.GetDevice(ctx, 3)
	if err != nil || dev.FwModel != "og" || dev.SleepStart != -1 || dev.SleepEnd != -1 {
		t.Errorf("device after migration: %+v, %v", dev, err)
	}
}

// dropButtons takes the columns of step 12 out of a database made from
// schema.sql, so that it has the shape from before that step.
var dropButtons = []string{
	`ALTER TABLE device DROP COLUMN button_command`,
	`ALTER TABLE device DROP COLUMN special_function`,
	`ALTER TABLE device_state DROP COLUMN last_special_function_sent`,
}

// TestMigrateButtons checks step 12: a database at version 11 gets the
// device's button columns, empty for the defaults, and the function last
// sent, empty for none recorded, keeps its devices and their state, and ends
// with the shape of schema.sql.
func TestMigrateButtons(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	want := shapeOf(t, openTest(t).Writer)

	path := filepath.Join(t.TempDir(), "v11.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range slices.Concat(dropRenderAttempt, dropRecipeRevision, dropRenderedSettingsAt, dropButtons, []string{
		`INSERT INTO device (id, mac, api_key_hash, friendly_id, fw_model, created_at) VALUES (3, 'D8:3B:DA:12:34:03', x'03', 'ABC003', 'og', 1)`,
		`INSERT INTO device_state (device_id, last_seen, last_refresh_rate_sent, last_image) VALUES (3, 5, 900, 'img')`,
		`PRAGMA user_version = 11`,
	}) {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 12 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	dev, err := client.Read.GetDevice(ctx, 3)
	if err != nil || dev.FwModel != "og" || dev.ButtonCommand != "" || dev.SpecialFunction != "" {
		t.Errorf("device after migration: %+v, %v", dev, err)
	}
	st, err := client.Read.GetDeviceStatus(ctx, 3)
	if err != nil || st.LastImage == nil || *st.LastImage != "img" || st.LastSpecialFunctionSent == nil || *st.LastSpecialFunctionSent != "" {
		t.Errorf("device state after migration: %+v, %v", st, err)
	}
}

// dropRenderAttempt takes the table and trigger of step 15 out of a
// database made from schema.sql, so that it has the shape from before that
// step.
var dropRenderAttempt = []string{
	`DROP TRIGGER render_attempt_device_deleted`,
	`DROP TABLE render_attempt`,
}

// TestMigrateRenderAttempt checks step 15: a database at version 14 gets
// the empty render_attempt table and its trigger, keeps its renders, and
// ends with the shape of schema.sql.
func TestMigrateRenderAttempt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	want := shapeOf(t, openTest(t).Writer)

	path := filepath.Join(t.TempDir(), "v14.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range slices.Concat(dropRenderAttempt, []string{
		`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (1, 'r', 's', 900, 1)`,
		`INSERT INTO plugin_instance (id, recipe_id, name, field_values, palette, created_at) VALUES (1, 1, 'i', '{}', '', 1)`,
		`INSERT INTO render (id, instance_id, width, height, palette, time_zone, locale, rendered_at, attempted_at, duration_ms) VALUES (1, 1, 800, 480, 'bw', 'UTC', 'en', 5, 5, 1)`,
		`PRAGMA user_version = 14`,
	}) {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 15 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if rows, err := client.Read.ListRenderAttempts(ctx); err != nil || len(rows) != 0 {
		t.Errorf("render attempts after migration: %+v, %v", rows, err)
	}
	if rows, err := client.Read.ListRenderRows(ctx); err != nil || len(rows) != 1 {
		t.Errorf("renders after migration: %d, %v", len(rows), err)
	}
}

// dropRecipeRevision takes the columns of step 14 out of a database made
// from schema.sql, so that it has the shape from before that step.
var dropRecipeRevision = []string{
	`ALTER TABLE recipe DROP COLUMN revision`,
	`ALTER TABLE recipe DROP COLUMN source_hash`,
	`ALTER TABLE recipe DROP COLUMN replaced_at`,
}

// TestMigrateRecipeRevision checks step 14: a database at version 13 gets
// the recipe's revision, 1, an empty source hash and replaced_at 0, keeps its
// recipes, and ends with the shape of schema.sql.
func TestMigrateRecipeRevision(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	want := shapeOf(t, openTest(t).Writer)

	path := filepath.Join(t.TempDir(), "v13.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range slices.Concat(dropRenderAttempt, dropRecipeRevision, []string{
		`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (4, 'r', 's', 900, 1)`,
		`PRAGMA user_version = 13`,
	}) {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 14 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	rec, err := client.Read.GetRecipe(ctx, 4)
	if err != nil || rec.Name != "r" || rec.Revision != 1 || rec.SourceHash != "" || rec.ReplacedAt != 0 {
		t.Errorf("recipe after migration: %+v, %v", rec, err)
	}
}

// dropRenderedSettingsAt takes the columns of step 13 out of a database
// made from schema.sql, so that it has the shape from before that step.
var dropRenderedSettingsAt = []string{
	`ALTER TABLE render DROP COLUMN rendered_settings_at`,
	`ALTER TABLE render DROP COLUMN failed_settings_at`,
	`ALTER TABLE render DROP COLUMN failed_error`,
}

// TestMigrateRenderedSettingsAt checks step 13: a database at version 12
// gets render.rendered_settings_at, which is the settings_at of a render
// with images and 0 for one that never had any, and failed_settings_at and
// failed_error, which are the settings_at and error of a render whose builds
// failed since its last good one, and ends with the shape of schema.sql.
func TestMigrateRenderedSettingsAt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	want := shapeOf(t, openTest(t).Writer)

	path := filepath.Join(t.TempDir(), "v12.db")
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range slices.Concat(dropRenderAttempt, dropRecipeRevision, dropRenderedSettingsAt, []string{
		`INSERT INTO recipe (id, name, source, refresh_interval, installed_at) VALUES (1, 'r', 's', 900, 1)`,
		`INSERT INTO plugin_instance (id, recipe_id, name, field_values, palette, created_at) VALUES (1, 1, 'i', '{}', '', 1)`,
		`INSERT INTO render (id, instance_id, width, height, palette, time_zone, locale, rendered_at, attempted_at, duration_ms, settings_at) VALUES (1, 1, 800, 480, 'bw', 'UTC', 'en', 5, 5, 1, 7)`,
		`INSERT INTO render (id, instance_id, width, height, palette, time_zone, locale, rendered_at, attempted_at, duration_ms, error, settings_at, failures) VALUES (2, 1, 400, 300, 'bw', 'UTC', 'en', 0, 5, 1, 'boom', 9, 2)`,
		`INSERT INTO render (id, instance_id, width, height, palette, time_zone, locale, rendered_at, attempted_at, duration_ms, error, settings_at, failures) VALUES (3, 1, 200, 100, 'bw', 'UTC', 'en', 4, 5, 1, 'busy', 8, 0)`,
		`PRAGMA user_version = 12`,
	}) {
		if _, err := old.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if v := userVersion(t, client.Writer); v != len(steps) || v < 13 {
		t.Errorf("version = %d, want %d", v, len(steps))
	}
	if got := shapeOf(t, client.Writer); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("migrated shape differs from schema.sql\nmigrated:\n%s\nfresh:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, tc := range []struct {
		id, rendered, failed int64
		failedError          string
	}{
		{1, 7, 0, ""},     // Its images were made from the settings it read.
		{2, 0, 9, "boom"}, // It never had images, and its builds failed.
		{3, 8, 0, ""},     // Its last build failed, but it did not count.
	} {
		var rendered, failed int64
		var failedError string
		err := client.Reader.QueryRowContext(ctx, `SELECT rendered_settings_at, failed_settings_at, failed_error FROM render WHERE id = ?`, tc.id).Scan(&rendered, &failed, &failedError)
		if err != nil || rendered != tc.rendered || failed != tc.failed || failedError != tc.failedError {
			t.Errorf("render %d: %d, %d, %q, %v; want %d, %d, %q", tc.id, rendered, failed, failedError, err, tc.rendered, tc.failed, tc.failedError)
		}
	}
}

// TestOpenExisting checks that OpenExisting refuses a missing database
// without creating the file or its directory, and opens one that Open made.
func TestOpenExisting(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()

	for _, path := range []string{
		filepath.Join(root, "absent", "trmnl.db"),
		filepath.Join(root, "trmnl.db"),
	} {
		if _, err := OpenExisting(ctx, path); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("OpenExisting(%s) = %v, want ErrNoDatabase", path, err)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("directory holds %v, %v after refused opens; want it empty", entries, err)
	}

	path := filepath.Join(root, "trmnl.db")
	created, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Write.SetSetting(ctx, SetSettingParams{Key: "k", Value: "v", UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	existing, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting of an existing database: %v", err)
	}
	defer existing.Close()
	if v, err := existing.Read.GetSetting(ctx, "k"); err != nil || v != "v" {
		t.Errorf("GetSetting = %q, %v; want v", v, err)
	}
}
