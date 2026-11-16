-- name: CreateDevice :one
INSERT INTO device (mac, api_key_hash, friendly_id, name, fw_model, fw_version, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetDeviceByFriendlyID :one
SELECT * FROM device WHERE friendly_id = ?;

-- name: SetScreenOverride :exec
UPDATE device SET screen_override = ?, screen_override_until = ? WHERE id = ?;

-- name: UpdateDeviceSettings :exec
-- Writes every setting an admin changes on a device at once, so a change of
-- several is one statement in the caller's transaction.
UPDATE device SET name = ?, updates_enabled = ?, fw_pin = ?, profile_override = ?, palette_override = ?, time_zone = ?, locale = ?, sleep_start = ?, sleep_end = ?, button_command = ?, special_function = ? WHERE id = ?;

-- name: GetDevice :one
SELECT * FROM device WHERE id = ?;

-- name: GetDeviceByMAC :one
SELECT * FROM device WHERE mac = ?;

-- name: GetDeviceByAPIKeyHash :one
SELECT * FROM device WHERE api_key_hash = ?;

-- name: SetDeviceAPIKeyHash :exec
UPDATE device SET api_key_hash = ? WHERE id = ?;

-- name: FriendlyIDExists :one
SELECT EXISTS (SELECT 1 FROM device WHERE friendly_id = ?);

-- name: UpdateDeviceFirmware :exec
-- An empty model or version keeps the stored one.
UPDATE device SET fw_model = COALESCE(NULLIF(CAST(sqlc.arg(fw_model) AS TEXT), ''), fw_model), fw_version = COALESCE(NULLIF(CAST(sqlc.arg(fw_version) AS TEXT), ''), fw_version) WHERE id = sqlc.arg(id);

-- name: UpsertDeviceState :exec
-- An empty last_image keeps the one stored, for an answer that sends the device
-- no image. An empty last_special_function_sent keeps the one stored, for an
-- answer the firmware does not save a special function from.
INSERT INTO device_state (device_id, last_seen, battery_v, rssi, wake_time_ms, update_source, last_refresh_rate_sent, last_image, last_special_function_sent, width, height)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (device_id) DO UPDATE SET
  last_seen = excluded.last_seen,
  battery_v = excluded.battery_v,
  rssi = excluded.rssi,
  wake_time_ms = excluded.wake_time_ms,
  update_source = excluded.update_source,
  last_refresh_rate_sent = excluded.last_refresh_rate_sent,
  last_image = COALESCE(NULLIF(excluded.last_image, ''), device_state.last_image),
  last_special_function_sent = COALESCE(NULLIF(excluded.last_special_function_sent, ''), device_state.last_special_function_sent),
  width = excluded.width,
  height = excluded.height;

-- name: SetDeviceCursor :exec
-- Moves a device's playlist cursor. It does not touch device_state, so it
-- says nothing about whether the device has woken.
INSERT INTO device_cursor (device_id, item_id, item_started_at)
VALUES (?, ?, ?)
ON CONFLICT (device_id) DO UPDATE SET
  item_id = excluded.item_id,
  item_started_at = excluded.item_started_at;

-- name: GetDeviceCursor :one
-- The playlist item a device is showing, and when it started. A device with
-- no cursor has no row.
SELECT item_id, item_started_at FROM device_cursor WHERE device_id = ?;

-- name: ListDeviceStatus :many
SELECT sqlc.embed(device), s.last_seen, s.battery_v, s.rssi, s.wake_time_ms, s.update_source, s.last_refresh_rate_sent, s.last_image, s.last_special_function_sent, s.width, s.height,
  EXISTS (SELECT 1 FROM blocked_mac WHERE blocked_mac.mac = device.mac) AS blocked
FROM device
LEFT JOIN device_state s ON s.device_id = device.id
ORDER BY device.name, device.id;

-- name: GetDeviceStatus :one
SELECT sqlc.embed(device), s.last_seen, s.battery_v, s.rssi, s.wake_time_ms, s.update_source, s.last_refresh_rate_sent, s.last_image, s.last_special_function_sent, s.width, s.height,
  EXISTS (SELECT 1 FROM blocked_mac WHERE blocked_mac.mac = device.mac) AS blocked
FROM device
LEFT JOIN device_state s ON s.device_id = device.id
WHERE device.id = ?;

-- name: ListDeviceNames :many
-- Every device's ID, friendly ID and name, for a list to choose one from.
SELECT id, friendly_id, name FROM device ORDER BY COALESCE(NULLIF(name, ''), friendly_id), id;
