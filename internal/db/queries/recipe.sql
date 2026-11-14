-- name: CreateRecipe :one
INSERT INTO recipe (name, source, refresh_interval, installed_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: DeleteRecipe :exec
DELETE FROM recipe WHERE id = ?;

-- name: ListRecipes :many
SELECT * FROM recipe ORDER BY id;

-- name: CreateInstance :one
INSERT INTO plugin_instance (recipe_id, name, field_values, secret_values, palette, created_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetInstance :one
SELECT * FROM plugin_instance WHERE id = ?;

-- name: ListInstances :many
SELECT plugin_instance.id, plugin_instance.name, plugin_instance.palette, recipe.id AS recipe_id, recipe.name AS recipe_name
FROM plugin_instance
JOIN recipe ON recipe.id = plugin_instance.recipe_id
ORDER BY plugin_instance.id;

-- name: GetRecipe :one
SELECT * FROM recipe WHERE id = ?;
