package db

import (
	"context"
	"database/sql"
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
}

// migrate brings the database to the latest version in one transaction, so a
// step that fails partway leaves the database as it was. The version is
// PRAGMA user_version, which is stored in the transaction.
//
// A new database, with no tables and version 0, gets schema.sql and is
// stamped with the latest version. A database with a version above the latest
// came from a newer server, and is refused.
func (c *Client) migrate(ctx context.Context) error {
	return c.InTx(ctx, func(q *Queries) error {
		var version, tables int
		if err := q.db.QueryRowContext(ctx, `SELECT user_version FROM pragma_user_version`).Scan(&version); err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		if version > len(steps) {
			return fmt.Errorf("database schema version %d is newer than this server's %d", version, len(steps))
		}
		if version == 0 {
			err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'`).Scan(&tables)
			if err != nil {
				return fmt.Errorf("inspect schema: %w", err)
			}
			if tables == 0 {
				if _, err := q.db.ExecContext(ctx, schema); err != nil {
					return fmt.Errorf("apply schema: %w", err)
				}
				return stamp(ctx, q.db, len(steps))
			}
		}
		for i := version; i < len(steps); i++ {
			if err := steps[i].run(ctx, q.db); err != nil {
				return fmt.Errorf("migrate to version %d (%s): %w", i+1, steps[i].name, err)
			}
		}
		return stamp(ctx, q.db, len(steps))
	})
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
