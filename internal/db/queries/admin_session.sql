-- name: CreateAdminSession :exec
INSERT INTO admin_session (token_hash, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?);

-- name: GetAdminSessionByHash :one
SELECT * FROM admin_session WHERE token_hash = ?;

-- name: TouchAdminSession :exec
UPDATE admin_session SET last_seen_at = ?, expires_at = ? WHERE id = ?;

-- name: DeleteAdminSessionByHash :exec
DELETE FROM admin_session WHERE token_hash = ?;

-- name: DeleteExpiredAdminSessions :exec
DELETE FROM admin_session WHERE expires_at <= ?;

-- name: DeleteAllAdminSessions :exec
DELETE FROM admin_session;
