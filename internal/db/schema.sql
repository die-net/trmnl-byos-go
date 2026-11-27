-- Runtime schema for trmnl-byos-go. sqlc generates queries against this
-- file, and Open applies it to a new
-- database, and migrate.go upgrades an old one. Times are Unix seconds.
--
-- Every surrogate id is AUTOINCREMENT, so an ID is never handed out twice.
-- Without it SQLite reuses the highest deleted ID, and a stale page, API
-- client or script holding that ID would act on the next row to take it.

-- A device is one display that has called /api/setup. mac is the ID header
-- it sent. api_key_hash is the SHA-256 of the key it got back, which it
-- sends as Access-Token to prove it is this device. The key itself is never
-- stored. fw_model is the Model header, the firmware's own name for its
-- board, which decides what firmware it may be offered.
CREATE TABLE IF NOT EXISTS device (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
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
  screen_override_until INTEGER NOT NULL DEFAULT 0,
  -- updates_enabled is 1 when the device may be offered firmware. It is 0
  -- until someone turns it on, so updates happen only when asked for.
  updates_enabled INTEGER NOT NULL DEFAULT 0,
  -- fw_pin is a firmware version the device stays on. A device with a pin is
  -- offered no firmware. Empty means no pin.
  fw_pin TEXT NOT NULL DEFAULT '',
  -- profile_override is the registry model the device's panel is, when its
  -- Model header says otherwise, as the 4-color OG on firmware 1.8.10 and
  -- older does. It sets what renders and system screens are drawn for, not
  -- the firmware the device is offered. The largest image the device takes
  -- never goes above its firmware's, and is the override model's when that
  -- is smaller. Empty means the Model header's.
  profile_override TEXT NOT NULL DEFAULT '',
  -- palette_override is the palette the device renders with when a plugin
  -- instance asks for none, one of its model's palettes. Empty means the
  -- model's default.
  palette_override TEXT NOT NULL DEFAULT '',
  -- time_zone is the IANA zone the device's schedules are read in and its
  -- renders are made for. Empty means the server's.
  time_zone TEXT NOT NULL DEFAULT '',
  -- locale is the language the device's renders are made for, one of
  -- trmnlp's locales. Empty means the server's locale setting.
  locale TEXT NOT NULL DEFAULT '',
  -- sleep_start and sleep_end bound the window in which the device is shown
  -- no playlist item, in minutes after local midnight in the device's time
  -- zone, 0 to 1439. The window wraps past midnight when the end is before
  -- the start. Both are -1 for no window, which is always awake.
  sleep_start INTEGER NOT NULL DEFAULT -1,
  sleep_end INTEGER NOT NULL DEFAULT -1,
  -- button_command is what a single button press does: next, previous, first
  -- or refresh. Empty means the default, next.
  button_command TEXT NOT NULL DEFAULT '',
  -- special_function is what a double-click does, one of the firmware's
  -- function names the server lets an admin choose. Empty means the default,
  -- rewind.
  special_function TEXT NOT NULL DEFAULT ''
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
  -- last_special_function_sent is the special_function in our last answer
  -- to a wake that asked for no special function, which is the function the
  -- firmware saves and runs at its next double-click. It is '' when no such
  -- answer was recorded.
  last_special_function_sent TEXT NOT NULL DEFAULT ''
);

-- device_cursor is where a device is in its playlist: item_id is the item it
-- is showing and item_started_at is when it started. A device that has none
-- has no row. An item that was deleted or is no longer in the device's
-- playlist starts the playlist over. It is the server's state, not
-- something the device reported, so it is apart from device_state, and a
-- row in one says nothing about the other.
CREATE TABLE IF NOT EXISTS device_cursor (
  device_id INTEGER PRIMARY KEY REFERENCES device (id) ON DELETE CASCADE,
  item_id INTEGER NOT NULL,
  item_started_at INTEGER NOT NULL
);

-- device_log holds entries a device sent to /api/log. device_at is the
-- device's own clock, which is 0 before it has synced time. detail is the
-- entry as the device sent it.
CREATE TABLE IF NOT EXISTS device_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id INTEGER NOT NULL REFERENCES device (id) ON DELETE CASCADE,
  received_at INTEGER NOT NULL,
  device_at INTEGER NOT NULL,
  level TEXT NOT NULL,
  message TEXT NOT NULL,
  detail TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS device_log_device ON device_log (device_id, id);

-- device_tombstone is the MAC of a deleted device, kept until that MAC
-- completes a new setup. The device still holds the key it had, and a
-- request from it with that key gets reset_firmware, which sends it back
-- through setup. Without the tombstone its MAC would be unknown, and an
-- unknown MAC is never reset, so the device would poll forever. A MAC is
-- never both a device and a tombstone.
CREATE TABLE IF NOT EXISTS device_tombstone (
  mac TEXT PRIMARY KEY,
  deleted_at INTEGER NOT NULL
);

-- blocked_mac is a MAC the server refuses, registered or not. A blocked
-- MAC gets a screen saying so from /api/display and /api/setup, even while
-- the join window is open, and is never reset. A registered device keeps
-- its row while blocked, but its key is refused and nothing is rendered
-- for it, so an unblock gives it back as it was. A block is by MAC, so it
-- outlives the device row, and it wins over a tombstone.
CREATE TABLE IF NOT EXISTS blocked_mac (
  mac TEXT PRIMARY KEY,
  blocked_at INTEGER NOT NULL
);

-- setting holds server-wide values, such as the join window.
CREATE TABLE IF NOT EXISTS setting (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- recipe is an installed TRMNL recipe. Its files are in recipes/<id>/ under
-- the data directory. name and refresh_interval come from its settings.yml.
CREATE TABLE IF NOT EXISTS recipe (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  -- source is the directory or zip file it was installed from.
  source TEXT NOT NULL,
  -- refresh_interval is how often, in seconds, an instance is rendered
  -- again.
  refresh_interval INTEGER NOT NULL,
  installed_at INTEGER NOT NULL,
  -- revision numbers the recipe's files, which are under
  -- recipes/<id>/<revision>/. A replace adds a revision and makes it this
  -- one in the same transaction, so the commit is the switch.
  revision INTEGER NOT NULL DEFAULT 1,
  -- source_hash is the hex SHA-256 of the installed files (recipe.TreeHash),
  -- so a replace with the same files changes nothing. It is '' for a recipe
  -- installed before it was recorded.
  source_hash TEXT NOT NULL DEFAULT '',
  -- replaced_at is when the files were last replaced, 0 if never.
  replaced_at INTEGER NOT NULL DEFAULT 0
);

-- plugin_instance is a recipe with its settings filled in: what a playlist
-- shows.
CREATE TABLE IF NOT EXISTS plugin_instance (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
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
  created_at INTEGER NOT NULL,
  -- settings_at is when the instance's field values or secrets last changed,
  -- 0 if they never have. It rises with every change, by at least a second,
  -- so a render that read the settings can tell newer ones from the ones it
  -- read. A rename or a palette change leaves it alone.
  settings_at INTEGER NOT NULL DEFAULT 0,
  -- secret_keys is a sorted JSON array of the keynames the admin marked
  -- secret, on top of the password fields, which always are. A marked
  -- field's value is in secret_values, never in field_values. The names are
  -- not sealed: they come from the recipe's settings.yml, which is not
  -- secret, and which store a value goes in must be known without the key.
  secret_keys TEXT NOT NULL DEFAULT '[]'
);

-- playlist is a list of items that devices rotate through.
--
-- days, window_start, window_end, starts_at and ends_at are its schedule, and
-- playlist_item has the same five columns for an item. days is a bitmask of
-- weekdays, bit 0 Sunday to bit 6 Saturday, and 127 is every day.
-- window_start and window_end are minutes after local midnight, 0 to 1439. Equal
-- means all day, and an end before the start wraps past midnight and belongs
-- to the day it starts on. starts_at and ends_at are Unix seconds, 0 for
-- unbounded, and the schedule is active in [starts_at, ends_at). The default
-- is always active.
CREATE TABLE IF NOT EXISTS playlist (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  days INTEGER NOT NULL DEFAULT 127,
  window_start INTEGER NOT NULL DEFAULT 0,
  window_end INTEGER NOT NULL DEFAULT 0,
  starts_at INTEGER NOT NULL DEFAULT 0,
  ends_at INTEGER NOT NULL DEFAULT 0
);

-- playlist_device links a device to its playlists. A device shows the first
-- one, by lowest priority, that is scheduled now and has an item to show.
-- priority is the playlist's index in the device's order, 0 to n-1 with no
-- gaps, and is unique per device.
CREATE TABLE IF NOT EXISTS playlist_device (
  device_id INTEGER NOT NULL REFERENCES device (id) ON DELETE CASCADE,
  playlist_id INTEGER NOT NULL REFERENCES playlist (id) ON DELETE CASCADE,
  priority INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (device_id, playlist_id)
);

CREATE INDEX IF NOT EXISTS playlist_device_playlist ON playlist_device (playlist_id);

CREATE UNIQUE INDEX IF NOT EXISTS playlist_device_priority ON playlist_device (device_id, priority);

-- playlist_item is one screen in a playlist. Items show in position order,
-- and position is the item's index in its playlist, 0 to n-1 with no gaps.
-- A hidden item is never shown, and its renders are not kept.
CREATE TABLE IF NOT EXISTS playlist_item (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  playlist_id INTEGER NOT NULL REFERENCES playlist (id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  -- layout is full for one instance on the whole screen, the only layout
  -- so far. A mashup's layout will place one instance per slot.
  layout TEXT NOT NULL DEFAULT 'full',
  -- duration is how long the item shows, in seconds. 0 shows it until a
  -- button press.
  duration INTEGER NOT NULL DEFAULT 0,
  hidden INTEGER NOT NULL DEFAULT 0,
  days INTEGER NOT NULL DEFAULT 127,
  window_start INTEGER NOT NULL DEFAULT 0,
  window_end INTEGER NOT NULL DEFAULT 0,
  starts_at INTEGER NOT NULL DEFAULT 0,
  ends_at INTEGER NOT NULL DEFAULT 0,
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

-- instance_data is the JSON object a script pushed to an instance, after
-- merging each push into the last. The renderer sees it as the top level of
-- .trmnlp.yml's variables. updated_at is when the latest push changed it,
-- in Unix seconds, and always rises with a push: a push in the same second
-- as the last one is stored one second later, so a build that recorded
-- the value it read can tell a newer push from the one it read.
CREATE TABLE IF NOT EXISTS instance_data (
  instance_id INTEGER PRIMARY KEY REFERENCES plugin_instance (id) ON DELETE CASCADE,
  data TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- render is the latest build of an instance at one size, palette, time zone
-- and locale, the render target. device_id is 0 for a render every device
-- shares, and a device's ID for a recipe that reads trmnl.device, which is
-- built once per device. It has no foreign key because 0 is no device; the
-- render_device_deleted trigger removes a deleted device's renders.
-- time_zone and locale are the ones in effect for the devices that show the
-- render, never empty, so a change of the server's time zone or locale makes
-- new targets.
-- rendered_at and the render_image rows are from the latest build that
-- succeeded. attempted_at, error and log are from the latest build, so an
-- error is kept next to the last good images.
CREATE TABLE IF NOT EXISTS render (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id INTEGER NOT NULL REFERENCES plugin_instance (id) ON DELETE CASCADE,
  device_id INTEGER NOT NULL DEFAULT 0,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  palette TEXT NOT NULL,
  time_zone TEXT NOT NULL,
  locale TEXT NOT NULL,
  -- rendered_at is when the latest successful build finished, 0 if none
  -- has.
  rendered_at INTEGER NOT NULL DEFAULT 0,
  attempted_at INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  -- error is why the latest build failed, empty if it succeeded.
  error TEXT NOT NULL DEFAULT '',
  -- log is the latest build's log, cut to a bound.
  log TEXT NOT NULL DEFAULT '',
  -- data_at is the updated_at of the instance_data the latest build read,
  -- 0 if it read none. The instance's data is newer than the render when
  -- its updated_at is greater.
  data_at INTEGER NOT NULL DEFAULT 0,
  -- failures is how many builds in a row have failed, 0 after a success.
  -- The scheduler waits longer to retry the more there are.
  failures INTEGER NOT NULL DEFAULT 0,
  -- settings_at is the settings_at of the plugin_instance row the latest
  -- build read. The instance's settings are newer than the render when its
  -- settings_at is greater.
  settings_at INTEGER NOT NULL DEFAULT 0,
  -- rendered_settings_at is the settings_at the latest successful build
  -- read, which the stored images were made from.
  rendered_settings_at INTEGER NOT NULL DEFAULT 0,
  -- failed_settings_at and failed_error are the settings_at and error of the
  -- latest failed build since the last good one that counts in failures, 0
  -- and '' if none has. A failure that does not count, such as an
  -- unreachable renderer, leaves them. When failed_settings_at is greater
  -- than rendered_settings_at, the instance's current settings failed.
  failed_settings_at INTEGER NOT NULL DEFAULT 0,
  failed_error TEXT NOT NULL DEFAULT '',
  UNIQUE (instance_id, device_id, width, height, palette, time_zone, locale)
);

CREATE TRIGGER IF NOT EXISTS render_device_deleted AFTER DELETE ON device
BEGIN
  DELETE FROM render WHERE device_id = OLD.id;
END;

-- render_attempt is a build that has started and not yet recorded its
-- outcome, one per render target, keyed as render is. The scheduler writes
-- it before the build, and deletes it in the transaction that saves the
-- outcome, or when the renderer did not take the build. It stays when the
-- server is stopping, since systemd stops a unit with SIGTERM after the
-- kernel killed one of its processes for memory as for a planned stop. One
-- left when the server starts means the server stopped during the build,
-- which the build may have caused, such as a sandboxed build whose process
-- the kernel killed for memory, which by default makes systemd stop the
-- whole unit. At startup each leftover becomes a counted failure of its
-- target, so the build waits out the backoff and is not run again at once.
-- started_at, data_at and settings_at are what that failure records: when
-- the build started, and the versions of the instance the scan saw. A
-- deleted instance takes its rows with it, and the
-- render_attempt_device_deleted trigger a deleted device's.
CREATE TABLE IF NOT EXISTS render_attempt (
  instance_id INTEGER NOT NULL REFERENCES plugin_instance (id) ON DELETE CASCADE,
  device_id INTEGER NOT NULL DEFAULT 0,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  palette TEXT NOT NULL,
  time_zone TEXT NOT NULL,
  locale TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  data_at INTEGER NOT NULL DEFAULT 0,
  settings_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (instance_id, device_id, width, height, palette, time_zone, locale)
);

CREATE TRIGGER IF NOT EXISTS render_attempt_device_deleted AFTER DELETE ON device
BEGIN
  DELETE FROM render_attempt WHERE device_id = OLD.id;
END;

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

-- api_token is a bearer token for the admin API. token_hash is the SHA-256
-- of the token, which is 256 random bits, so a fast hash is enough. The
-- token itself is shown once, when it is created, and never stored.
-- last_used_at is 0 until the token is first used.
CREATE TABLE IF NOT EXISTS api_token (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0
);

-- admin_session is a signed-in browser. token_hash is the SHA-256 of the
-- session token, which is 256 random bits, so a fast hash is enough. The
-- token itself is only in the browser's cookie. last_seen_at and expires_at
-- slide forward as the session is used. Setting the admin password deletes
-- every session.
CREATE TABLE IF NOT EXISTS admin_session (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

-- firmware is a build in the catalog. It is for exactly one firmware model,
-- the Model header value, and sha256 (lowercase hex) is both its file name
-- under firmware/ and the name in its download URL. chip and idf_version are
-- read from the file's headers when it is added. source says where it came
-- from, such as upload. A model has one build per version.
CREATE TABLE IF NOT EXISTS firmware (
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

-- firmware_offer counts how many times a build was offered to a device, so
-- a build that never takes, such as a failing download or a file whose real
-- version is not the one it was added as, is not offered forever.
CREATE TABLE IF NOT EXISTS firmware_offer (
  device_id INTEGER NOT NULL REFERENCES device (id) ON DELETE CASCADE,
  firmware_id INTEGER NOT NULL REFERENCES firmware (id) ON DELETE CASCADE,
  offers INTEGER NOT NULL,
  last_offered_at INTEGER NOT NULL,
  PRIMARY KEY (device_id, firmware_id)
);

-- firmware_rollout offers a build, first to canary_device_id and, once that
-- device reports the build's version, to every device of the same model.
-- state is active or canceled. A build with no active rollout is offered to
-- no one and its file is not served.
CREATE TABLE IF NOT EXISTS firmware_rollout (
  firmware_id INTEGER PRIMARY KEY REFERENCES firmware (id) ON DELETE CASCADE,
  canary_device_id INTEGER NOT NULL REFERENCES device (id) ON DELETE CASCADE,
  state TEXT NOT NULL,
  started_at INTEGER NOT NULL
);
