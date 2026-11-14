-- name: GetInstanceData :one
SELECT * FROM instance_data WHERE instance_id = ?;

-- name: SaveInstanceData :exec
INSERT INTO instance_data (instance_id, data, updated_at) VALUES (?, ?, ?)
ON CONFLICT (instance_id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at;
