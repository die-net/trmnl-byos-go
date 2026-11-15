-- name: CreateFirmware :one
INSERT INTO firmware (fw_model, version, sha256, chip, idf_version, bytes, source, uploaded_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: FirmwareExists :one
-- Whether the catalog has this file, or has a build of this model at this
-- version.
SELECT EXISTS (SELECT 1 FROM firmware WHERE sha256 = ? OR (fw_model = ? AND version = ?));

-- name: GetFirmware :one
SELECT * FROM firmware WHERE id = ?;

-- name: ListFirmware :many
-- Every build with its rollout, if any, and the canary device's state.
SELECT sqlc.embed(firmware), r.state AS rollout_state, r.started_at AS rollout_started_at,
  c.id AS canary_id, c.friendly_id AS canary, c.fw_model AS canary_model, c.fw_version AS canary_version,
  CAST(COALESCE(o.offers, 0) AS INTEGER) AS canary_offers,
  CAST(COALESCE(o.last_offered_at, 0) AS INTEGER) AS canary_last_offered_at
FROM firmware
LEFT JOIN firmware_rollout r ON r.firmware_id = firmware.id
LEFT JOIN device c ON c.id = r.canary_device_id
LEFT JOIN firmware_offer o ON o.firmware_id = firmware.id AND o.device_id = r.canary_device_id
ORDER BY firmware.fw_model, firmware.id;

-- name: GetFirmwareEntry :one
-- One build with its rollout, in the shape of a ListFirmware row.
SELECT sqlc.embed(firmware), r.state AS rollout_state, r.started_at AS rollout_started_at,
  c.id AS canary_id, c.friendly_id AS canary, c.fw_model AS canary_model, c.fw_version AS canary_version,
  CAST(COALESCE(o.offers, 0) AS INTEGER) AS canary_offers,
  CAST(COALESCE(o.last_offered_at, 0) AS INTEGER) AS canary_last_offered_at
FROM firmware
LEFT JOIN firmware_rollout r ON r.firmware_id = firmware.id
LEFT JOIN device c ON c.id = r.canary_device_id
LEFT JOIN firmware_offer o ON o.firmware_id = firmware.id AND o.device_id = r.canary_device_id
WHERE firmware.id = ?;

-- name: ListFirmwareDevices :many
-- The devices whose last reported model is the given one, by friendly ID,
-- with how many times each was offered a build.
SELECT sqlc.embed(device),
  CAST(COALESCE(o.offers, 0) AS INTEGER) AS offers,
  CAST(COALESCE(o.last_offered_at, 0) AS INTEGER) AS last_offered_at
FROM device
LEFT JOIN firmware_offer o ON o.device_id = device.id AND o.firmware_id = sqlc.arg(firmware_id)
WHERE device.fw_model = sqlc.arg(fw_model)
ORDER BY device.friendly_id;

-- name: ListActiveFirmware :many
-- The builds of a model in an active rollout, with the canary device's
-- model, version and offer count, and how many times the given device was
-- offered each build.
SELECT sqlc.embed(firmware), r.canary_device_id, c.fw_model AS canary_model, c.fw_version AS canary_version,
  CAST(COALESCE(co.offers, 0) AS INTEGER) AS canary_offers,
  CAST(COALESCE(o.offers, 0) AS INTEGER) AS offers
FROM firmware
JOIN firmware_rollout r ON r.firmware_id = firmware.id AND r.state = 'active'
JOIN device c ON c.id = r.canary_device_id
LEFT JOIN firmware_offer co ON co.firmware_id = firmware.id AND co.device_id = r.canary_device_id
LEFT JOIN firmware_offer o ON o.firmware_id = firmware.id AND o.device_id = sqlc.arg(device_id)
WHERE firmware.fw_model = sqlc.arg(fw_model);

-- name: RecordFirmwareOffer :exec
INSERT INTO firmware_offer (device_id, firmware_id, offers, last_offered_at)
VALUES (?, ?, 1, ?)
ON CONFLICT (device_id, firmware_id) DO UPDATE SET
  offers = offers + 1,
  last_offered_at = excluded.last_offered_at;

-- name: ResetFirmwareOffers :exec
DELETE FROM firmware_offer WHERE device_id = ? AND firmware_id = ?;

-- name: GetActiveFirmwareBySHA :one
-- A build in an active rollout, by the hash in its download URL.
SELECT firmware.* FROM firmware
JOIN firmware_rollout r ON r.firmware_id = firmware.id AND r.state = 'active'
WHERE firmware.sha256 = ?;

-- name: StartFirmwareRollout :exec
-- Starts a rollout, or restarts it with another canary.
INSERT INTO firmware_rollout (firmware_id, canary_device_id, state, started_at)
VALUES (?, ?, 'active', ?)
ON CONFLICT (firmware_id) DO UPDATE SET
  canary_device_id = excluded.canary_device_id,
  state = 'active',
  started_at = excluded.started_at;

-- name: CancelFirmwareRollout :execrows
UPDATE firmware_rollout SET state = 'canceled' WHERE firmware_id = ? AND state = 'active';

-- name: SetDeviceFirmwarePolicy :exec
UPDATE device SET updates_enabled = ?, fw_pin = ? WHERE id = ?;
