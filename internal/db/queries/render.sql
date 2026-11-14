-- name: ListRenderTargets :many
-- Every instance in a playlist linked to a device, with what the device
-- reported about itself, from which its render size and palette follow.
SELECT DISTINCT plugin_instance.id AS instance_id, plugin_instance.palette, recipe.refresh_interval,
  device.fw_model, device_state.width, device_state.height
FROM playlist_device
JOIN device ON device.id = playlist_device.device_id
LEFT JOIN device_state ON device_state.device_id = device.id
JOIN playlist_item ON playlist_item.playlist_id = playlist_device.playlist_id
JOIN playlist_slot ON playlist_slot.item_id = playlist_item.id
JOIN plugin_instance ON plugin_instance.id = playlist_slot.instance_id
JOIN recipe ON recipe.id = plugin_instance.recipe_id;

-- name: GetRender :one
SELECT * FROM render
WHERE instance_id = ? AND width = ? AND height = ? AND palette = ?;

-- name: SaveRenderSuccess :one
INSERT INTO render (instance_id, width, height, palette, rendered_at, attempted_at, duration_ms, error, log)
VALUES (?, ?, ?, ?, ?, ?, ?, '', ?)
ON CONFLICT (instance_id, width, height, palette) DO UPDATE SET
  rendered_at = excluded.rendered_at,
  attempted_at = excluded.attempted_at,
  duration_ms = excluded.duration_ms,
  error = '',
  log = excluded.log
RETURNING id;

-- name: SaveRenderFailure :exec
-- Records a failed build, keeping the last good images and their time.
INSERT INTO render (instance_id, width, height, palette, rendered_at, attempted_at, duration_ms, error, log)
VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)
ON CONFLICT (instance_id, width, height, palette) DO UPDATE SET
  attempted_at = excluded.attempted_at,
  duration_ms = excluded.duration_ms,
  error = excluded.error,
  log = excluded.log;

-- name: SaveRenderImage :exec
INSERT INTO render_image (render_id, layout, image, bytes)
VALUES (?, ?, ?, ?)
ON CONFLICT (render_id, layout) DO UPDATE SET image = excluded.image, bytes = excluded.bytes;

-- name: GetItemScreen :one
-- What a device showing an instance gets: the instance's name and refresh
-- interval, and the render for the device's size and palette, if any.
SELECT plugin_instance.name, recipe.refresh_interval,
  render.rendered_at, render.attempted_at, render.error,
  render_image.image, render_image.bytes
FROM plugin_instance
JOIN recipe ON recipe.id = plugin_instance.recipe_id
LEFT JOIN render ON render.instance_id = plugin_instance.id
  AND render.width = sqlc.arg(width) AND render.height = sqlc.arg(height) AND render.palette = sqlc.arg(palette)
LEFT JOIN render_image ON render_image.render_id = render.id AND render_image.layout = sqlc.arg(layout)
WHERE plugin_instance.id = sqlc.arg(instance_id);
