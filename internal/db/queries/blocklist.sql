-- name: DeleteDevice :exec
-- Deletes a device. Its state, cursor, logs, playlist links and firmware
-- offers cascade, a rollout it is the canary of goes with it, and the
-- render_device_deleted trigger deletes its renders.
DELETE FROM device WHERE id = ?;

-- name: AddDeviceTombstone :exec
INSERT INTO device_tombstone (mac, deleted_at) VALUES (?, ?)
ON CONFLICT (mac) DO UPDATE SET deleted_at = excluded.deleted_at;

-- name: DeleteDeviceTombstone :exec
DELETE FROM device_tombstone WHERE mac = ?;

-- name: GetMACStanding :one
-- What the server knows of a MAC: whether it is blocked, the MAC of a
-- deleted device, or a registered device.
SELECT
  EXISTS (SELECT 1 FROM blocked_mac WHERE blocked_mac.mac = sqlc.arg(mac)) AS blocked,
  EXISTS (SELECT 1 FROM device_tombstone WHERE device_tombstone.mac = sqlc.arg(mac)) AS deleted,
  EXISTS (SELECT 1 FROM device WHERE device.mac = sqlc.arg(mac)) AS registered;

-- name: BlockMAC :one
-- Blocks a MAC. A MAC already blocked keeps the time it was first blocked.
INSERT INTO blocked_mac (mac, blocked_at) VALUES (?, ?)
ON CONFLICT (mac) DO UPDATE SET blocked_at = blocked_mac.blocked_at
RETURNING *;

-- name: UnblockMAC :execrows
DELETE FROM blocked_mac WHERE mac = ?;

-- name: ListCanaryFirmware :many
-- The firmware builds whose rollout has the device as its canary, which a
-- delete of the device deletes.
SELECT firmware_id FROM firmware_rollout WHERE canary_device_id = ? ORDER BY firmware_id;

-- name: ListBlockedMACs :many
-- Every blocked MAC, with the device that has it, if one does, and whether
-- it is a deleted device's tombstone.
SELECT blocked_mac.mac, blocked_mac.blocked_at, device.friendly_id, device.name,
  EXISTS (SELECT 1 FROM device_tombstone WHERE device_tombstone.mac = blocked_mac.mac) AS deleted
FROM blocked_mac
LEFT JOIN device ON device.mac = blocked_mac.mac
ORDER BY blocked_mac.mac;
