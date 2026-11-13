-- Runtime schema for trmnl-byos-go. sqlc generates queries against this
-- file, and Open applies it at startup. Times are Unix seconds.

-- A device is one display that has called /api/setup. mac is the ID header
-- it sent. api_key_hash is the SHA-256 of the key it got back, which it
-- sends as Access-Token to prove it is this device. The key itself is never
-- stored. fw_model is the Model header, the firmware's own name for its
-- board, which decides what firmware it may be offered.
CREATE TABLE IF NOT EXISTS device (
  id INTEGER PRIMARY KEY,
  mac TEXT NOT NULL UNIQUE,
  api_key_hash BLOB NOT NULL UNIQUE,
  friendly_id TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT '',
  fw_model TEXT NOT NULL DEFAULT '',
  fw_version TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

-- device_state is what a device reported on its last /api/display call, and
-- what we told it. A column is NULL when the device did not send that header.
CREATE TABLE IF NOT EXISTS device_state (
  device_id INTEGER PRIMARY KEY REFERENCES device (id) ON DELETE CASCADE,
  last_seen INTEGER NOT NULL,
  -- battery_v is the Battery-Voltage header, in volts.
  battery_v REAL,
  rssi INTEGER,
  -- wake_time_ms is the Wake-Time header: how long the previous wake took.
  wake_time_ms INTEGER,
  -- update_source is the Update-Source header, such as timer or button.
  update_source TEXT,
  -- last_refresh_rate_sent is the refresh_rate in our last response, in
  -- seconds, so last_seen plus it is when the device should wake next.
  last_refresh_rate_sent INTEGER NOT NULL,
  -- last_image is the name of the image in our last response. Image expiry
  -- keeps every image a row names.
  last_image TEXT NOT NULL DEFAULT ''
);

-- device_log holds entries a device sent to /api/log. device_at is the
-- device's own clock, which is 0 before it has synced time. detail is the
-- entry as the device sent it.
CREATE TABLE IF NOT EXISTS device_log (
  id INTEGER PRIMARY KEY,
  device_id INTEGER NOT NULL REFERENCES device (id) ON DELETE CASCADE,
  received_at INTEGER NOT NULL,
  device_at INTEGER NOT NULL,
  level TEXT NOT NULL,
  message TEXT NOT NULL,
  detail TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS device_log_device ON device_log (device_id, id);

-- setting holds server-wide values, such as the join window.
CREATE TABLE IF NOT EXISTS setting (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
