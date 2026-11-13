-- name: CreateDevice :one
INSERT INTO device (mac, api_key, friendly_id, name, fw_model, fw_version, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetDeviceByMAC :one
SELECT * FROM device WHERE mac = ?;

-- name: ListDevices :many
SELECT * FROM device ORDER BY name, id;

-- name: UpdateDeviceFirmware :exec
UPDATE device SET fw_model = ?, fw_version = ? WHERE id = ?;
