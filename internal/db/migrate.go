package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
)

// A step moves a database from the version before it to its own. Step i of
// steps is version i+1. A step is a Go function when it needs logic, such as
// a probe, and otherwise a function that runs a constant SQL string with
// tx.ExecContext (both drivers run every statement of a multi-statement Exec). sqlc
// cannot take a migration, because it generates queries against the finished
// schema (CLAUDE.md), so the SQL lives here and not in queries/.
type step struct {
	name string
	run  func(ctx context.Context, tx DBTX) error
}

// steps is the ordered list of migrations. A change to schema.sql that an
// existing database must get is a new step at the end, and the same change
// in schema.sql. TestMigrateMatchesSchema fails when the two disagree. Never
// edit or reorder a step that has shipped.
var steps = []step{
	{"adopt a database from before numbered versions", adoptUnversioned},
	{"move playlist cursors out of device_state", moveCursors},
	{"add admin sessions", addAdminSessions},
	{"add playlist schedules and hidden items", addPlaylistSchedules},
	{"make playlist priorities a unique index per device", addPlaylistPriorityIndex},
	{"add settings_at to instances and renders, and secret marks to instances", addSettingsAt},
	{"never reuse a surrogate ID", useAutoincrement},
	{"add device tombstones and the MAC blocklist", addBlocklist},
	{"add per-device profile and palette overrides", addProfileOverride},
	{"add per-device time zone and locale, and key renders by them", addZoneLocale},
	{"add per-device sleep windows", addSleepWindow},
	{"add per-device button command and double-click function, and the function last sent", addButtons},
	{"record the settings a render's images were made from", addRenderedSettingsAt},
}

// migrate brings the database to the latest version in one transaction, so a
// step that fails partway leaves the database as it was. The version is
// PRAGMA user_version, which is stored in the transaction.
//
// A new database, with no tables and version 0, gets schema.sql and is
// stamped with the latest version. A database with a version above the latest
// came from a newer server, and is refused.
//
// It runs with foreign keys off, as SQLite documents for a step that rebuilds
// a table. PRAGMA foreign_keys cannot change inside a transaction, and with
// it on, DROP TABLE does an implicit DELETE that fires ON DELETE CASCADE on
// the rows of every child table. So it takes the writer's one connection,
// turns foreign keys off, runs the steps in a transaction, and, when a step
// ran, runs PRAGMA foreign_key_check before it commits, refusing a database
// where any row has no parent. The check reads every row of every child
// table, so it is not run on the usual start, when there is nothing to do. Foreign keys go back on before the connection returns to
// the pool, on every path. If that fails, the connection is discarded, and
// the pool opens a new one from the DSN, which has them on.
func (c *Client) migrate(ctx context.Context) (err error) {
	conn, err := c.Writer.Conn(ctx)
	if err != nil {
		return fmt.Errorf("take migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("turn foreign keys off: %w", err)
	}
	defer func() {
		// A canceled ctx must not leave foreign keys off.
		if _, rerr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`); rerr != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("turn foreign keys on: %w", rerr))
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stepped, err := migrateTx(ctx, tx)
	if err != nil {
		return err
	}
	if stepped {
		if err := checkForeignKeys(ctx, tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// migrateTx applies schema.sql to a new database, or the steps after its
// version to an old one, and stamps the latest version. stepped is whether
// any step ran.
func migrateTx(ctx context.Context, tx DBTX) (stepped bool, err error) {
	var version, tables int
	if err := tx.QueryRowContext(ctx, `SELECT user_version FROM pragma_user_version`).Scan(&version); err != nil {
		return false, fmt.Errorf("read schema version: %w", err)
	}
	if version > len(steps) {
		return false, fmt.Errorf("database schema version %d is newer than this server's %d", version, len(steps))
	}
	if version == len(steps) {
		return false, nil
	}
	if version == 0 {
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'`).Scan(&tables)
		if err != nil {
			return false, fmt.Errorf("inspect schema: %w", err)
		}
		if tables == 0 {
			if _, err := tx.ExecContext(ctx, schema); err != nil {
				return false, fmt.Errorf("apply schema: %w", err)
			}
			return false, stamp(ctx, tx, len(steps))
		}
	}
	for i := version; i < len(steps); i++ {
		if err := steps[i].run(ctx, tx); err != nil {
			return false, fmt.Errorf("migrate to version %d (%s): %w", i+1, steps[i].name, err)
		}
	}
	return true, stamp(ctx, tx, len(steps))
}

// checkForeignKeys fails if any row refers to a parent that is not there. It
// runs after the steps, with foreign keys off, so it finds what the steps
// broke or what the database already had.
func checkForeignKeys(ctx context.Context, tx DBTX) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("check foreign keys: %w", err)
		}
		return fmt.Errorf("foreign key check failed: row %d of %s has no row in %s", rowid.Int64, table, parent)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	return nil
}

// stamp records version. PRAGMA takes no bound parameters, so the number is
// formatted in. It is an int from this package, never input.
func stamp(ctx context.Context, tx DBTX, version int) error {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("stamp schema version: %w", err)
	}
	return nil
}

// addedColumns are the columns that were added to a table after it first
// shipped, with the ALTER TABLE that adds each. Step 1 uses them to upgrade a
// database made before versions were numbered, which cannot say which of them
// it has. Each definition matches the column's in baselineSchema.
var addedColumns = []struct {
	table, column, alter string
}{
	{"device", "screen_override", `ALTER TABLE device ADD COLUMN screen_override TEXT NOT NULL DEFAULT ''`},
	{"device", "screen_override_until", `ALTER TABLE device ADD COLUMN screen_override_until INTEGER NOT NULL DEFAULT 0`},
	{"device", "updates_enabled", `ALTER TABLE device ADD COLUMN updates_enabled INTEGER NOT NULL DEFAULT 0`},
	{"device", "fw_pin", `ALTER TABLE device ADD COLUMN fw_pin TEXT NOT NULL DEFAULT ''`},
	{"device_state", "width", `ALTER TABLE device_state ADD COLUMN width INTEGER`},
	{"device_state", "height", `ALTER TABLE device_state ADD COLUMN height INTEGER`},
	{"device_state", "item_id", `ALTER TABLE device_state ADD COLUMN item_id INTEGER`},
	{"device_state", "item_started_at", `ALTER TABLE device_state ADD COLUMN item_started_at INTEGER`},
	{"render", "data_at", `ALTER TABLE render ADD COLUMN data_at INTEGER NOT NULL DEFAULT 0`},
	{"render", "failures", `ALTER TABLE render ADD COLUMN failures INTEGER NOT NULL DEFAULT 0`},
}

// adoptUnversioned is step 1. It takes a database whose tables exist and whose
// version is 0, which is every database made before versions were numbered,
// to the shape of baselineSchema. Those databases came from servers that ran
// schema.sql at every start and added columns on demand, so the shape of each
// table is unknown and is probed. This is the only place probes of an existing
// shape are allowed. Once the database is stamped, they never run again.
// It is a Go function because it needs those probes. A database it cannot
// bring to the baseline, such as one with a device table from the first
// commit, is refused with an error that names the first difference, and the
// transaction rolls back before the version is stamped.
func adoptUnversioned(ctx context.Context, tx DBTX) error {
	if err := dropOldRenders(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, baselineSchema); err != nil {
		return fmt.Errorf("apply baseline schema: %w", err)
	}
	for _, col := range addedColumns {
		if err := addColumn(ctx, tx, col.table, col.column, col.alter); err != nil {
			return err
		}
	}
	return checkBaselineShape(ctx, tx)
}

// addColumn runs alter, which adds column to table, unless the table has the
// column already.
func addColumn(ctx context.Context, tx DBTX, table, column, alter string) error {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	if n > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, alter); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// dropRenders drops the render tables so baselineSchema can create them in
// their current shape. render_image goes first because it refers to render.
const dropRenders = `DROP TABLE IF EXISTS render_image;
DROP TABLE IF EXISTS render`

// dropOldRenders drops the render tables if render predates its device_id
// column, which SQLite cannot add to the table's UNIQUE constraint. The
// rows are only built images, which the scheduler builds again, and image
// expiry removes the ones no row names.
func dropOldRenders(ctx context.Context, tx DBTX) error {
	var cols, withDevice int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(name = 'device_id'), 0) FROM pragma_table_info('render')`).Scan(&cols, &withDevice)
	if err != nil {
		return fmt.Errorf("inspect render: %w", err)
	}
	if cols == 0 || withDevice > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, dropRenders); err != nil {
		return fmt.Errorf("drop old renders: %w", err)
	}
	return nil
}

// moveCursorsSQL is step 2. It copies each cursor that has both columns set
// into device_cursor, then drops the columns from device_state. A device
// whose state row existed only to hold a cursor (written with a made-up
// last_seen and a refresh of 0, before device_cursor) loses that row too,
// since it never woke. Its last_refresh_rate_sent of 0 is what marks it,
// because a real response always sends a refresh_rate of at least a
// minute.
const moveCursorsSQL = `CREATE TABLE device_cursor (
  device_id INTEGER PRIMARY KEY REFERENCES device (id) ON DELETE CASCADE,
  item_id INTEGER NOT NULL,
  item_started_at INTEGER NOT NULL
);
INSERT INTO device_cursor (device_id, item_id, item_started_at)
  SELECT device_id, item_id, item_started_at FROM device_state
  WHERE item_id IS NOT NULL AND item_started_at IS NOT NULL;
DELETE FROM device_state WHERE last_refresh_rate_sent = 0;
ALTER TABLE device_state DROP COLUMN item_id;
ALTER TABLE device_state DROP COLUMN item_started_at`

// moveCursors is step 2: see moveCursorsSQL.
func moveCursors(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, moveCursorsSQL); err != nil {
		return fmt.Errorf("move cursors: %w", err)
	}
	return nil
}

// addAdminSessionsSQL is step 3: the table that holds admin sessions, as in
// schema.sql.
const addAdminSessionsSQL = `CREATE TABLE IF NOT EXISTS admin_session (
  id INTEGER PRIMARY KEY,
  token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
)`

// addAdminSessions is step 3: see addAdminSessionsSQL.
func addAdminSessions(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, addAdminSessionsSQL); err != nil {
		return fmt.Errorf("add admin_session: %w", err)
	}
	return nil
}

// addPlaylistSchedulesSQL is step 4. It adds the schedule columns to playlist
// and playlist_item and the hidden column to playlist_item, as in schema.sql,
// and makes each playlist's positions run 0 to n-1 in their old order, since
// the API now treats position as an index. A position is unique within its
// playlist and SQLite checks that per row, so the positions are first made
// negative, which no index is, and then ranked by their old order, which is
// descending now.
const addPlaylistSchedulesSQL = `ALTER TABLE playlist ADD COLUMN days INTEGER NOT NULL DEFAULT 127;
ALTER TABLE playlist ADD COLUMN window_start INTEGER NOT NULL DEFAULT 0;
ALTER TABLE playlist ADD COLUMN window_end INTEGER NOT NULL DEFAULT 0;
ALTER TABLE playlist ADD COLUMN starts_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE playlist ADD COLUMN ends_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE playlist_item ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0;
ALTER TABLE playlist_item ADD COLUMN days INTEGER NOT NULL DEFAULT 127;
ALTER TABLE playlist_item ADD COLUMN window_start INTEGER NOT NULL DEFAULT 0;
ALTER TABLE playlist_item ADD COLUMN window_end INTEGER NOT NULL DEFAULT 0;
ALTER TABLE playlist_item ADD COLUMN starts_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE playlist_item ADD COLUMN ends_at INTEGER NOT NULL DEFAULT 0;
UPDATE playlist_item SET position = -1 - position;
UPDATE playlist_item SET position = ranked.idx
  FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY playlist_id ORDER BY position DESC) - 1 AS idx FROM playlist_item) AS ranked
  WHERE ranked.id = playlist_item.id`

// addPlaylistSchedules is step 4: see addPlaylistSchedulesSQL.
func addPlaylistSchedules(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, addPlaylistSchedulesSQL); err != nil {
		return fmt.Errorf("add playlist schedules: %w", err)
	}
	return nil
}

// addPlaylistPrioritySQL is step 5. It renumbers each device's playlist links
// to 0 to n-1 in their old order (priority, then playlist ID), since the API
// now treats priority as an index, and then adds the unique index that keeps
// it so. The rank is computed before any row changes, so the unique index
// does not exist yet to collide with it.
const addPlaylistPrioritySQL = `UPDATE playlist_device SET priority = ranked.idx
  FROM (SELECT device_id, playlist_id, ROW_NUMBER() OVER (PARTITION BY device_id ORDER BY priority, playlist_id) - 1 AS idx FROM playlist_device) AS ranked
  WHERE ranked.device_id = playlist_device.device_id AND ranked.playlist_id = playlist_device.playlist_id;
CREATE UNIQUE INDEX playlist_device_priority ON playlist_device (device_id, priority)`

// addPlaylistPriorityIndex is step 5: see addPlaylistPrioritySQL.
func addPlaylistPriorityIndex(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, addPlaylistPrioritySQL); err != nil {
		return fmt.Errorf("add playlist priority index: %w", err)
	}
	return nil
}

// addSettingsAtSQL is step 6. It adds settings_at to plugin_instance and
// render, and secret_keys to plugin_instance, as in schema.sql. Every
// existing settings_at is 0, so no render is made stale by the step, and no
// instance has a field marked secret.
const addSettingsAtSQL = `ALTER TABLE plugin_instance ADD COLUMN settings_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plugin_instance ADD COLUMN secret_keys TEXT NOT NULL DEFAULT '[]';
ALTER TABLE render ADD COLUMN settings_at INTEGER NOT NULL DEFAULT 0`

// addSettingsAt is step 6: see addSettingsAtSQL.
func addSettingsAt(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, addSettingsAtSQL); err != nil {
		return fmt.Errorf("add settings_at and secret_keys: %w", err)
	}
	return nil
}

// useAutoincrementSQL is step 7. It rebuilds every table with a surrogate
// id INTEGER PRIMARY KEY as the same table with AUTOINCREMENT, so an ID is
// never handed out twice and a stale page, API client or script holding a
// deleted ID cannot reach a newer row. Without AUTOINCREMENT, SQLite gives
// a new row the highest ID plus one, which reuses the ID of a deleted
// highest row. SQLite cannot add AUTOINCREMENT to a table, so each is made
// anew, filled with the old rows and their IDs, and renamed over the old one,
// the way the SQLite documentation describes. The column lists are explicit
// because a migrated table's column order can differ from a new one's.
// Every table's definition is as in schema.sql at this version. Tables whose
// key is a foreign key or is composite have no surrogate ID and are not
// touched.
//
// The trigger goes first because ALTER TABLE RENAME fails while a trigger
// body names a table that is dropped, and is created again at the end. Each
// table's indexes go with its DROP TABLE and are made again after its rename.
// migrate runs the step with foreign keys off, so the drops do not cascade.
//
// Inserting the old rows with their IDs sets each table's sqlite_sequence
// entry to its highest ID, so new rows start above it. An ID that was deleted
// before the migration, and was above the highest ID then, was never recorded
// and can be handed out once more.
//
// sqlc cannot take this, because it is a migration (CLAUDE.md).
const useAutoincrementSQL = `DROP TRIGGER IF EXISTS render_device_deleted;

CREATE TABLE device_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  mac TEXT NOT NULL UNIQUE,
  api_key_hash BLOB NOT NULL UNIQUE,
  friendly_id TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT '',
  fw_model TEXT NOT NULL DEFAULT '',
  fw_version TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  screen_override TEXT NOT NULL DEFAULT '',
  screen_override_until INTEGER NOT NULL DEFAULT 0,
  updates_enabled INTEGER NOT NULL DEFAULT 0,
  fw_pin TEXT NOT NULL DEFAULT ''
);
INSERT INTO device_new (id, mac, api_key_hash, friendly_id, name, fw_model, fw_version, created_at, screen_override, screen_override_until, updates_enabled, fw_pin)
  SELECT id, mac, api_key_hash, friendly_id, name, fw_model, fw_version, created_at, screen_override, screen_override_until, updates_enabled, fw_pin FROM device;
DROP TABLE device;
ALTER TABLE device_new RENAME TO device;

CREATE TABLE device_log_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id INTEGER NOT NULL REFERENCES device (id) ON DELETE CASCADE,
  received_at INTEGER NOT NULL,
  device_at INTEGER NOT NULL,
  level TEXT NOT NULL,
  message TEXT NOT NULL,
  detail TEXT NOT NULL
);
INSERT INTO device_log_new (id, device_id, received_at, device_at, level, message, detail)
  SELECT id, device_id, received_at, device_at, level, message, detail FROM device_log;
DROP TABLE device_log;
ALTER TABLE device_log_new RENAME TO device_log;
CREATE INDEX device_log_device ON device_log (device_id, id);

CREATE TABLE recipe_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  source TEXT NOT NULL,
  refresh_interval INTEGER NOT NULL,
  installed_at INTEGER NOT NULL
);
INSERT INTO recipe_new (id, name, source, refresh_interval, installed_at)
  SELECT id, name, source, refresh_interval, installed_at FROM recipe;
DROP TABLE recipe;
ALTER TABLE recipe_new RENAME TO recipe;

CREATE TABLE plugin_instance_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  recipe_id INTEGER NOT NULL REFERENCES recipe (id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  field_values TEXT NOT NULL DEFAULT '{}',
  secret_values BLOB,
  palette TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  settings_at INTEGER NOT NULL DEFAULT 0,
  secret_keys TEXT NOT NULL DEFAULT '[]'
);
INSERT INTO plugin_instance_new (id, recipe_id, name, field_values, secret_values, palette, created_at, settings_at, secret_keys)
  SELECT id, recipe_id, name, field_values, secret_values, palette, created_at, settings_at, secret_keys FROM plugin_instance;
DROP TABLE plugin_instance;
ALTER TABLE plugin_instance_new RENAME TO plugin_instance;

CREATE TABLE playlist_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  days INTEGER NOT NULL DEFAULT 127,
  window_start INTEGER NOT NULL DEFAULT 0,
  window_end INTEGER NOT NULL DEFAULT 0,
  starts_at INTEGER NOT NULL DEFAULT 0,
  ends_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO playlist_new (id, name, days, window_start, window_end, starts_at, ends_at)
  SELECT id, name, days, window_start, window_end, starts_at, ends_at FROM playlist;
DROP TABLE playlist;
ALTER TABLE playlist_new RENAME TO playlist;

CREATE TABLE playlist_item_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  playlist_id INTEGER NOT NULL REFERENCES playlist (id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  layout TEXT NOT NULL DEFAULT 'full',
  duration INTEGER NOT NULL DEFAULT 0,
  hidden INTEGER NOT NULL DEFAULT 0,
  days INTEGER NOT NULL DEFAULT 127,
  window_start INTEGER NOT NULL DEFAULT 0,
  window_end INTEGER NOT NULL DEFAULT 0,
  starts_at INTEGER NOT NULL DEFAULT 0,
  ends_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE (playlist_id, position)
);
INSERT INTO playlist_item_new (id, playlist_id, position, layout, duration, hidden, days, window_start, window_end, starts_at, ends_at)
  SELECT id, playlist_id, position, layout, duration, hidden, days, window_start, window_end, starts_at, ends_at FROM playlist_item;
DROP TABLE playlist_item;
ALTER TABLE playlist_item_new RENAME TO playlist_item;

CREATE TABLE render_new (
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
);
INSERT INTO render_new (id, instance_id, device_id, width, height, palette, rendered_at, attempted_at, duration_ms, error, log, data_at, failures, settings_at)
  SELECT id, instance_id, device_id, width, height, palette, rendered_at, attempted_at, duration_ms, error, log, data_at, failures, settings_at FROM render;
DROP TABLE render;
ALTER TABLE render_new RENAME TO render;

CREATE TABLE api_token_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO api_token_new (id, name, token_hash, created_at, last_used_at)
  SELECT id, name, token_hash, created_at, last_used_at FROM api_token;
DROP TABLE api_token;
ALTER TABLE api_token_new RENAME TO api_token;

CREATE TABLE admin_session_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
INSERT INTO admin_session_new (id, token_hash, created_at, last_seen_at, expires_at)
  SELECT id, token_hash, created_at, last_seen_at, expires_at FROM admin_session;
DROP TABLE admin_session;
ALTER TABLE admin_session_new RENAME TO admin_session;

CREATE TABLE firmware_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  fw_model TEXT NOT NULL,
  version TEXT NOT NULL,
  sha256 TEXT NOT NULL UNIQUE,
  chip TEXT NOT NULL,
  idf_version TEXT NOT NULL,
  bytes INTEGER NOT NULL,
  source TEXT NOT NULL,
  uploaded_at INTEGER NOT NULL,
  UNIQUE (fw_model, version)
);
INSERT INTO firmware_new (id, fw_model, version, sha256, chip, idf_version, bytes, source, uploaded_at)
  SELECT id, fw_model, version, sha256, chip, idf_version, bytes, source, uploaded_at FROM firmware;
DROP TABLE firmware;
ALTER TABLE firmware_new RENAME TO firmware;

CREATE TRIGGER render_device_deleted AFTER DELETE ON device
BEGIN
  DELETE FROM render WHERE device_id = OLD.id;
END`

// useAutoincrement is step 7: see useAutoincrementSQL.
func useAutoincrement(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, useAutoincrementSQL); err != nil {
		return fmt.Errorf("use autoincrement: %w", err)
	}
	return nil
}

// addBlocklistSQL is step 8: the device_tombstone and blocked_mac tables,
// as in schema.sql.
const addBlocklistSQL = `CREATE TABLE IF NOT EXISTS device_tombstone (
  mac TEXT PRIMARY KEY,
  deleted_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS blocked_mac (
  mac TEXT PRIMARY KEY,
  blocked_at INTEGER NOT NULL
)`

// addBlocklist is step 8: see addBlocklistSQL.
func addBlocklist(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, addBlocklistSQL); err != nil {
		return fmt.Errorf("add blocklist: %w", err)
	}
	return nil
}

// addProfileOverrideSQL is step 9: the device's profile and palette
// overrides, as in schema.sql, empty in every existing row.
const addProfileOverrideSQL = `ALTER TABLE device ADD COLUMN profile_override TEXT NOT NULL DEFAULT '';
ALTER TABLE device ADD COLUMN palette_override TEXT NOT NULL DEFAULT ''`

// addProfileOverride is step 9: see addProfileOverrideSQL.
func addProfileOverride(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, addProfileOverrideSQL); err != nil {
		return fmt.Errorf("add profile overrides: %w", err)
	}
	return nil
}

// addZoneLocaleSQL is step 10. It adds device.time_zone and device.locale,
// empty in every existing row, and puts time_zone and locale in the render
// table's key, so a render is per time zone and locale as well as per size
// and palette. SQLite cannot change a table's UNIQUE constraint, so render is
// made anew, filled with the old rows and their IDs, and renamed over the old
// one, as step 7 did, with explicit column lists. render_image refers to
// render by name, so it keeps working, and its rows are kept.
//
// Copying the rows with their IDs sets render_new's sqlite_sequence to the
// highest ID left, and dropping render deletes its own, which may be higher:
// the ID of a render deleted since. So render's counter is carried over to
// render_new before the drop, the higher of the two, and the rename keeps
// it. Otherwise a deleted render's ID would be handed out again, which
// AUTOINCREMENT is there to stop.
//
// An existing render gets time_zone and locale of the empty string. A target
// always has the zone and locale in effect, never an empty one, so no target
// matches such a row. The scheduler's pruning deletes it, with its image
// rows, and builds the render again once. There are no deployments, so
// nothing is made to keep the rows.
//
// The trigger goes first and is made again at the end, for the reason given
// at useAutoincrementSQL. migrate runs the step with foreign keys off, so
// the drop does not cascade to render_image.
//
// sqlc cannot take this, because it is a migration (CLAUDE.md).
const addZoneLocaleSQL = `DROP TRIGGER IF EXISTS render_device_deleted;

ALTER TABLE device ADD COLUMN time_zone TEXT NOT NULL DEFAULT '';
ALTER TABLE device ADD COLUMN locale TEXT NOT NULL DEFAULT '';

CREATE TABLE render_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id INTEGER NOT NULL REFERENCES plugin_instance (id) ON DELETE CASCADE,
  device_id INTEGER NOT NULL DEFAULT 0,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  palette TEXT NOT NULL,
  time_zone TEXT NOT NULL,
  locale TEXT NOT NULL,
  rendered_at INTEGER NOT NULL DEFAULT 0,
  attempted_at INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  log TEXT NOT NULL DEFAULT '',
  data_at INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,
  settings_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE (instance_id, device_id, width, height, palette, time_zone, locale)
);
INSERT INTO render_new (id, instance_id, device_id, width, height, palette, time_zone, locale, rendered_at, attempted_at, duration_ms, error, log, data_at, failures, settings_at)
  SELECT id, instance_id, device_id, width, height, palette, '', '', rendered_at, attempted_at, duration_ms, error, log, data_at, failures, settings_at FROM render;
UPDATE sqlite_sequence SET seq = MAX(seq, COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'render'), 0))
  WHERE name = 'render_new';
INSERT INTO sqlite_sequence (name, seq)
  SELECT 'render_new', seq FROM sqlite_sequence
  WHERE name = 'render' AND NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'render_new');
DROP TABLE render;
ALTER TABLE render_new RENAME TO render;

CREATE TRIGGER render_device_deleted AFTER DELETE ON device
BEGIN
  DELETE FROM render WHERE device_id = OLD.id;
END`

// addZoneLocale is step 10: see addZoneLocaleSQL.
func addZoneLocale(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, addZoneLocaleSQL); err != nil {
		return fmt.Errorf("add time zone and locale: %w", err)
	}
	return nil
}

// addSleepColumnsSQL is step 11: the device's sleep window, as in schema.sql,
// -1 in both columns of every existing row, for no window.
const addSleepColumnsSQL = `ALTER TABLE device ADD COLUMN sleep_start INTEGER NOT NULL DEFAULT -1;
ALTER TABLE device ADD COLUMN sleep_end INTEGER NOT NULL DEFAULT -1`

// addSleepWindow is step 11: see addSleepColumnsSQL.
func addSleepWindow(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, addSleepColumnsSQL); err != nil {
		return fmt.Errorf("add sleep windows: %w", err)
	}
	return nil
}

// addButtonColumnsSQL is step 12: the device's button command and
// double-click function, and the double-click function last sent to it, as
// in schema.sql, empty in every existing row, for the defaults and for none
// recorded.
const addButtonColumnsSQL = `ALTER TABLE device ADD COLUMN button_command TEXT NOT NULL DEFAULT '';
ALTER TABLE device ADD COLUMN special_function TEXT NOT NULL DEFAULT '';
ALTER TABLE device_state ADD COLUMN last_special_function_sent TEXT NOT NULL DEFAULT ''`

// addButtons is step 12: see addButtonColumnsSQL.
func addButtons(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, addButtonColumnsSQL); err != nil {
		return fmt.Errorf("add button settings: %w", err)
	}
	return nil
}

// addRenderedSettingsAtSQL is step 13: render.rendered_settings_at, as in
// schema.sql. A row with images gets its settings_at, which is what its
// images were made from unless a build failed since, and then the guess
// keeps showing the images, as before the step.
const addRenderedSettingsAtSQL = `ALTER TABLE render ADD COLUMN rendered_settings_at INTEGER NOT NULL DEFAULT 0;
UPDATE render SET rendered_settings_at = settings_at WHERE rendered_at != 0`

// addRenderedSettingsAt is step 13: see addRenderedSettingsAtSQL.
func addRenderedSettingsAt(ctx context.Context, tx DBTX) error {
	if _, err := tx.ExecContext(ctx, addRenderedSettingsAtSQL); err != nil {
		return fmt.Errorf("add rendered_settings_at: %w", err)
	}
	return nil
}

// schemaShape lists what a database holds: every object in sqlite_master, with
// the SQL of indexes and triggers, which an ALTER TABLE does not change, and
// each table's columns and foreign keys. A table's own SQL is left out,
// because ADD COLUMN edits its text and not its meaning. Step 1 compares two
// shapes, and so do the migration tests.
func schemaShape(ctx context.Context, db DBTX) ([]string, error) {
	type object struct{ typ, name, tbl, sql string }
	var objects []object
	rows, err := db.QueryContext(ctx, `SELECT type, name, tbl_name, CASE WHEN type = 'table' THEN '' ELSE COALESCE(sql, '') END FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("list schema objects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.typ, &o.name, &o.tbl, &o.sql); err != nil {
			return nil, fmt.Errorf("list schema objects: %w", err)
		}
		objects = append(objects, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list schema objects: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list schema objects: %w", err)
	}
	var shape []string
	for _, o := range objects {
		shape = append(shape, strings.Join([]string{o.typ, o.name, o.tbl, strings.Join(strings.Fields(o.sql), " ")}, "|"))
		if o.typ != "table" {
			continue
		}
		cols, err := tableShape(ctx, db, o.name)
		if err != nil {
			return nil, err
		}
		shape = append(shape, cols...)
	}
	return shape, nil
}

// tableShape lists the columns and foreign keys of table, one line each.
func tableShape(ctx context.Context, db DBTX, table string) ([]string, error) {
	var shape []string
	cols, err := db.QueryContext(ctx, `SELECT cid, name, type, "notnull", COALESCE(dflt_value, 'NULL'), pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("list columns of %s: %w", table, err)
	}
	defer cols.Close()
	for cols.Next() {
		var cid, notNull, pk int
		var name, typ, dflt string
		if err := cols.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("list columns of %s: %w", table, err)
		}
		shape = append(shape, fmt.Sprintf("  %s col %d %s %s notnull=%d default=%s pk=%d", table, cid, name, typ, notNull, dflt, pk))
	}
	if err := cols.Err(); err != nil {
		return nil, fmt.Errorf("list columns of %s: %w", table, err)
	}
	fks, err := db.QueryContext(ctx, `SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("list foreign keys of %s: %w", table, err)
	}
	defer fks.Close()
	for fks.Next() {
		var ref, from, to, onDelete string
		if err := fks.Scan(&ref, &from, &to, &onDelete); err != nil {
			return nil, fmt.Errorf("list foreign keys of %s: %w", table, err)
		}
		shape = append(shape, fmt.Sprintf("  %s fk %s -> %s.%s %s", table, from, ref, to, onDelete))
	}
	if err := fks.Err(); err != nil {
		return nil, fmt.Errorf("list foreign keys of %s: %w", table, err)
	}
	return shape, nil
}

// baselineShape is the shape of a new database made from baselineSchema,
// built in memory.
func baselineShape(ctx context.Context) ([]string, error) {
	mem, err := sql.Open(driverName(), ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open baseline database: %w", err)
	}
	defer mem.Close()
	// Each connection to :memory: is its own database.
	mem.SetMaxOpenConns(1)
	if _, err := mem.ExecContext(ctx, baselineSchema); err != nil {
		return nil, fmt.Errorf("build baseline database: %w", err)
	}
	return schemaShape(ctx, mem)
}

// checkBaselineShape fails if tx does not hold exactly the tables, columns,
// indexes and foreign keys of baselineSchema, and names the first line that
// differs, so the error points at a table or column.
func checkBaselineShape(ctx context.Context, tx DBTX) error {
	want, err := baselineShape(ctx)
	if err != nil {
		return err
	}
	got, err := schemaShape(ctx, tx)
	if err != nil {
		return err
	}
	for i := 0; i < len(got) || i < len(want); i++ {
		var g, w string
		if i < len(got) {
			g = got[i]
		}
		if i < len(want) {
			w = want[i]
		}
		if g != w {
			return fmt.Errorf("database cannot be brought to the baseline schema: it has %q where the baseline has %q", strings.TrimSpace(g), strings.TrimSpace(w))
		}
	}
	return nil
}
