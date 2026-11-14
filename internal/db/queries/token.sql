-- name: CreateToken :one
INSERT INTO api_token (name, token_hash, created_at) VALUES (?, ?, ?) RETURNING *;

-- name: TokenNameExists :one
SELECT EXISTS (SELECT 1 FROM api_token WHERE name = ?);

-- name: GetTokenByHash :one
SELECT * FROM api_token WHERE token_hash = ?;

-- name: ListTokens :many
SELECT * FROM api_token ORDER BY name;

-- name: DeleteToken :execrows
DELETE FROM api_token WHERE name = ?;

-- name: TouchToken :exec
UPDATE api_token SET last_used_at = ? WHERE id = ?;
