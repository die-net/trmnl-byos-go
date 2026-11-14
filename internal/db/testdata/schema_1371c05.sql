-- Runtime schema for trmnl-byos-go. sqlc generates queries against this
-- file, and Open applies it at startup. Times are Unix seconds.

-- A device is one display that has called /api/setup. mac is the ID header
-- it sent, which picks the row; api_key is the token it got back, which
-- proves it is that device. fw_model is the Model header, the firmware's own
-- name for its board, which decides what firmware it may be offered.
CREATE TABLE IF NOT EXISTS device (
  id INTEGER PRIMARY KEY,
  mac TEXT NOT NULL UNIQUE,
  api_key TEXT NOT NULL UNIQUE,
  friendly_id TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT '',
  fw_model TEXT NOT NULL DEFAULT '',
  fw_version TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

-- setting holds server-wide values set from the admin UI, such as the
-- setup join window.
CREATE TABLE IF NOT EXISTS setting (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
