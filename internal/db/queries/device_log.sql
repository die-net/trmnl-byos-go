-- name: InsertDeviceLog :exec
INSERT INTO device_log (device_id, received_at, device_at, level, message, detail)
VALUES (?, ?, ?, ?, ?, ?);

-- name: TrimDeviceLog :exec
-- Keeps a device's newest keep entries. The subquery finds the
-- newest entry past those, and it and everything older go.
DELETE FROM device_log
WHERE device_log.device_id = sqlc.arg(device_id)
  AND device_log.id <= (
    SELECT newer.id FROM device_log AS newer
    WHERE newer.device_id = sqlc.arg(device_id)
    ORDER BY newer.id DESC
    LIMIT 1 OFFSET sqlc.arg(keep)
  );

-- name: ListDeviceLog :many
SELECT * FROM device_log
WHERE device_id = ?
ORDER BY id DESC
LIMIT ?;
