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
  created_at INTEGER NOT NULL,
  -- screen_override names a system screen, such as calibrate, that the
  -- device shows instead of its usual one until screen_override_until.
  screen_override TEXT NOT NULL DEFAULT '',
  screen_override_until INTEGER NOT NULL DEFAULT 0
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
  last_image TEXT NOT NULL DEFAULT '',
  -- width and height are the Width and Height headers, the panel size the
  -- firmware reports, which renders for a model the server does not know
  -- are drawn at.
  width INTEGER,
  height INTEGER,
  -- item_id is the playlist item the device is showing, and
  -- item_started_at is when it started. NULL before the device has shown
  -- one. An item that was deleted or is no longer in the device's playlist
  -- starts the playlist over.
  item_id INTEGER,
  item_started_at INTEGER
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

-- recipe is an installed TRMNL recipe. Its files are in recipes/<id>/ under
-- the data directory. name and refresh_interval come from its settings.yml.
CREATE TABLE IF NOT EXISTS recipe (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  -- source is the directory or zip file it was installed from.
  source TEXT NOT NULL,
  -- refresh_interval is how often, in seconds, an instance is rendered
  -- again.
  refresh_interval INTEGER NOT NULL,
  installed_at INTEGER NOT NULL
);

-- plugin_instance is a recipe with its settings filled in: what a playlist
-- shows.
CREATE TABLE IF NOT EXISTS plugin_instance (
  id INTEGER PRIMARY KEY,
  recipe_id INTEGER NOT NULL REFERENCES recipe (id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  -- field_values is a JSON object of the custom field values that are not
  -- secret, by keyname.
  field_values TEXT NOT NULL DEFAULT '{}',
  -- secret_values is a JSON object of secret field values, by keyname,
  -- sealed with the secrets key. NULL when there are none.
  secret_values BLOB,
  -- palette is the palette the instance asks for, such as bw to keep
  -- partial refreshes. Empty, or one the device cannot show, means the
  -- device's default.
  palette TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

-- playlist is a list of items that devices rotate through.
CREATE TABLE IF NOT EXISTS playlist (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);

-- playlist_device links a device to its playlists. The one with the lowest
-- priority is the one it shows.
CREATE TABLE IF NOT EXISTS playlist_device (
  device_id INTEGER NOT NULL REFERENCES device (id) ON DELETE CASCADE,
  playlist_id INTEGER NOT NULL REFERENCES playlist (id) ON DELETE CASCADE,
  priority INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (device_id, playlist_id)
);

CREATE INDEX IF NOT EXISTS playlist_device_playlist ON playlist_device (playlist_id);

-- playlist_item is one screen in a playlist. Items show in position order.
CREATE TABLE IF NOT EXISTS playlist_item (
  id INTEGER PRIMARY KEY,
  playlist_id INTEGER NOT NULL REFERENCES playlist (id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  -- layout is full for one instance on the whole screen, the only layout
  -- so far. A mashup's layout will place one instance per slot.
  layout TEXT NOT NULL DEFAULT 'full',
  -- duration is how long the item shows, in seconds. 0 shows it until a
  -- button press.
  duration INTEGER NOT NULL DEFAULT 0,
  UNIQUE (playlist_id, position)
);

-- playlist_slot is the instance shown in each slot of an item's layout. A
-- full item has slot 0 only.
CREATE TABLE IF NOT EXISTS playlist_slot (
  item_id INTEGER NOT NULL REFERENCES playlist_item (id) ON DELETE CASCADE,
  slot INTEGER NOT NULL,
  instance_id INTEGER NOT NULL REFERENCES plugin_instance (id) ON DELETE CASCADE,
  PRIMARY KEY (item_id, slot)
);

CREATE INDEX IF NOT EXISTS playlist_slot_instance ON playlist_slot (instance_id);

-- render is the latest build of an instance at one size and palette, the
-- render target. rendered_at and the render_image rows are from the latest
-- build that succeeded. attempted_at, error and log are from the latest
-- build, so an error is kept next to the last good images.
CREATE TABLE IF NOT EXISTS render (
  id INTEGER PRIMARY KEY,
  instance_id INTEGER NOT NULL REFERENCES plugin_instance (id) ON DELETE CASCADE,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  palette TEXT NOT NULL,
  -- rendered_at is when the latest successful build finished, 0 if none
  -- has.
  rendered_at INTEGER NOT NULL DEFAULT 0,
  attempted_at INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  -- error is why the latest build failed, empty if it succeeded.
  error TEXT NOT NULL DEFAULT '',
  -- log is the latest build's log, cut to a bound.
  log TEXT NOT NULL DEFAULT '',
  UNIQUE (instance_id, width, height, palette)
);

-- render_image is one layout's image from a render.
CREATE TABLE IF NOT EXISTS render_image (
  render_id INTEGER NOT NULL REFERENCES render (id) ON DELETE CASCADE,
  layout TEXT NOT NULL,
  image TEXT NOT NULL,
  -- bytes is the image's size, which the device's limit is checked
  -- against.
  bytes INTEGER NOT NULL,
  PRIMARY KEY (render_id, layout)
);
