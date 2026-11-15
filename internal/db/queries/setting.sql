-- name: GetSetting :one
SELECT value FROM setting WHERE key = ?;

-- name: SetSetting :exec
INSERT INTO setting (key, value, updated_at)
VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at;

-- name: DeleteSetting :exec
DELETE FROM setting WHERE key = ?;
