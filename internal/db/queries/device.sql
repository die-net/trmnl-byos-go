-- name: CreateDevice :one
INSERT INTO device (mac, api_key_hash, friendly_id, name, fw_model, fw_version, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetDeviceByMAC :one
SELECT * FROM device WHERE mac = ?;

-- name: GetDeviceByAPIKeyHash :one
SELECT * FROM device WHERE api_key_hash = ?;

-- name: SetDeviceAPIKeyHash :exec
UPDATE device SET api_key_hash = ? WHERE id = ?;

-- name: FriendlyIDExists :one
SELECT EXISTS (SELECT 1 FROM device WHERE friendly_id = ?);

-- name: ListDevices :many
SELECT * FROM device ORDER BY name, id;

-- name: UpdateDeviceFirmware :exec
UPDATE device SET fw_model = ?, fw_version = ? WHERE id = ?;

-- name: UpsertDeviceState :exec
INSERT INTO device_state (device_id, last_seen, battery_v, rssi, wake_time_ms, update_source, last_refresh_rate_sent, last_image)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (device_id) DO UPDATE SET
  last_seen = excluded.last_seen,
  battery_v = excluded.battery_v,
  rssi = excluded.rssi,
  wake_time_ms = excluded.wake_time_ms,
  update_source = excluded.update_source,
  last_refresh_rate_sent = excluded.last_refresh_rate_sent,
  last_image = excluded.last_image;

-- name: ListDeviceStatus :many
SELECT sqlc.embed(device), s.last_seen, s.battery_v, s.rssi, s.wake_time_ms, s.update_source, s.last_refresh_rate_sent
FROM device
LEFT JOIN device_state s ON s.device_id = device.id
ORDER BY device.name, device.id;
