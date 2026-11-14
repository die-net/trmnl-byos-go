-- name: CreateRecipe :one
INSERT INTO recipe (name, source, refresh_interval, installed_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: ListRecipes :many
SELECT * FROM recipe ORDER BY id;

-- name: CreateInstance :one
INSERT INTO plugin_instance (recipe_id, name, field_values, secret_values, palette, created_at)
VALUES (?, ?, ?, ?, ?, ?)
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
