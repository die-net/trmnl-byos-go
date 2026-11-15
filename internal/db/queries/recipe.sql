-- name: CreateRecipe :one
INSERT INTO recipe (name, source, refresh_interval, installed_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: ListRecipes :many
SELECT * FROM recipe ORDER BY id;

-- name: CreateInstance :one
INSERT INTO plugin_instance (recipe_id, name, field_values, secret_values, secret_keys, palette, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: SetInstanceSecrets :exec
-- Stores an instance's sealed secrets, which are bound to its ID and so can
-- be sealed only after the row exists.
UPDATE plugin_instance SET secret_values = ? WHERE id = ?;

-- name: GetInstance :one
SELECT * FROM plugin_instance WHERE id = ?;

-- name: ListInstances :many
SELECT sqlc.embed(plugin_instance), recipe.name AS recipe_name
FROM plugin_instance
JOIN recipe ON recipe.id = plugin_instance.recipe_id
ORDER BY plugin_instance.id;

-- name: GetRecipe :one
SELECT * FROM recipe WHERE id = ?;

-- name: DeleteInstance :execrows
-- Also deletes the instance's renders and its playlist slots, by cascade.
DELETE FROM plugin_instance WHERE id = ?;

-- name: ListInstancePlaylists :many
-- For every instance, the playlists that show it in any slot, each once, for
-- listing.
SELECT DISTINCT playlist_slot.instance_id, playlist.id AS playlist_id, playlist.name AS playlist_name
FROM playlist_slot
JOIN playlist_item ON playlist_item.id = playlist_slot.item_id
JOIN playlist ON playlist.id = playlist_item.playlist_id
ORDER BY playlist_slot.instance_id, playlist.name;

-- name: ListPlaylistsOfInstance :many
-- The playlists that show one instance in any slot, each once.
SELECT DISTINCT playlist.id AS playlist_id, playlist.name AS playlist_name
FROM playlist_slot
JOIN playlist_item ON playlist_item.id = playlist_slot.item_id
JOIN playlist ON playlist.id = playlist_item.playlist_id
WHERE playlist_slot.instance_id = ?
ORDER BY playlist.name;

-- name: UpdateInstance :one
-- Writes every setting of an instance, which the caller has read and merged
-- in the same transaction. settings_at is the caller's, since it rises only
-- when the field values or secrets changed.
UPDATE plugin_instance
SET name = ?, palette = ?, field_values = ?, secret_values = ?, secret_keys = ?, settings_at = ?
WHERE id = ?
RETURNING *;
