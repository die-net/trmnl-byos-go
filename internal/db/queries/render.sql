-- name: ListRenderTargets :many
-- Every instance in a visible item of a playlist linked to a device that is
-- not blocked, with what the device reported about itself, from which its
-- render size and palette follow, and its time zone and locale settings,
-- which are empty for the server's.
-- A recipe's device_id is the device the render is for, which the caller
-- drops for a recipe whose renders every device shares. data_at is when
-- the instance's pushed data last changed, 0 if it has none. settings_at is
-- when its field values last changed.
SELECT DISTINCT plugin_instance.id AS instance_id, plugin_instance.palette, recipe.id AS recipe_id,
  recipe.refresh_interval, device.id AS device_id, device.fw_model, device.profile_override, device.palette_override, device.time_zone, device.locale, device_state.width, device_state.height,
  device_state.device_id IS NOT NULL AS woken,
  CAST(COALESCE(instance_data.updated_at, 0) AS INTEGER) AS data_at,
  plugin_instance.settings_at
FROM playlist_device
JOIN device ON device.id = playlist_device.device_id
LEFT JOIN device_state ON device_state.device_id = device.id
JOIN playlist_item ON playlist_item.playlist_id = playlist_device.playlist_id
JOIN playlist_slot ON playlist_slot.item_id = playlist_item.id
JOIN plugin_instance ON plugin_instance.id = playlist_slot.instance_id
JOIN recipe ON recipe.id = plugin_instance.recipe_id
LEFT JOIN instance_data ON instance_data.instance_id = plugin_instance.id
WHERE playlist_item.hidden = 0
  AND NOT EXISTS (SELECT 1 FROM blocked_mac WHERE blocked_mac.mac = device.mac);

-- name: ListDeviceWakes :many
-- What the scheduler needs to know about each device that has woken and is
-- not blocked: its size, when it last woke and how long it was told to
-- sleep, its playlist cursor, and its time zone and locale settings.
SELECT device.id AS device_id, device.fw_model, device.profile_override, device.palette_override, device.time_zone, device.locale, device_state.width, device_state.height,
  device_state.last_seen, device_state.last_refresh_rate_sent, device_cursor.item_id, device_cursor.item_started_at
FROM device_state
JOIN device ON device.id = device_state.device_id
LEFT JOIN device_cursor ON device_cursor.device_id = device.id
WHERE NOT EXISTS (SELECT 1 FROM blocked_mac WHERE blocked_mac.mac = device.mac);

-- name: ListShownItems :many
-- The items of every playlist each device is linked to, as ListDeviceItems
-- gives them for one device, with the instance's recipe.
SELECT device.id AS device_id, playlist.id AS playlist_id, playlist.days AS playlist_days, playlist.window_start AS playlist_window_start,
  playlist.window_end AS playlist_window_end, playlist.starts_at AS playlist_starts_at, playlist.ends_at AS playlist_ends_at,
  playlist_item.id, playlist_item.duration, playlist_item.hidden, playlist_item.days AS item_days,
  playlist_item.window_start AS item_window_start, playlist_item.window_end AS item_window_end,
  playlist_item.starts_at AS item_starts_at, playlist_item.ends_at AS item_ends_at,
  playlist_slot.instance_id, plugin_instance.palette, recipe.id AS recipe_id, recipe.refresh_interval
FROM device
JOIN playlist_device ON playlist_device.device_id = device.id
JOIN playlist ON playlist.id = playlist_device.playlist_id
JOIN playlist_item ON playlist_item.playlist_id = playlist.id
JOIN playlist_slot ON playlist_slot.item_id = playlist_item.id AND playlist_slot.slot = 0
JOIN plugin_instance ON plugin_instance.id = playlist_slot.instance_id
JOIN recipe ON recipe.id = plugin_instance.recipe_id
ORDER BY device.id, playlist_device.priority, playlist_item.position;

-- name: GetRenderDevice :one
-- A device's values as of its last wake, which a per-device render shows
-- the recipe as trmnl.device.
SELECT device.friendly_id, device.fw_version, device_state.battery_v, device_state.rssi,
  device_state.last_refresh_rate_sent
FROM device
LEFT JOIN device_state ON device_state.device_id = device.id
WHERE device.id = ?;

-- name: ListRenderRows :many
-- Every render's row without its build log, which is large, for the
-- scheduler to decide in one read what is due.
SELECT id, instance_id, device_id, width, height, palette, time_zone, locale, rendered_at, attempted_at, duration_ms, error, data_at, settings_at, failures
FROM render;

-- name: SaveRenderSuccess :one
INSERT INTO render (instance_id, device_id, width, height, palette, time_zone, locale, rendered_at, attempted_at, duration_ms, error, log, data_at, settings_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)
ON CONFLICT (instance_id, device_id, width, height, palette, time_zone, locale) DO UPDATE SET
  rendered_at = excluded.rendered_at,
  attempted_at = excluded.attempted_at,
  duration_ms = excluded.duration_ms,
  error = '',
  log = excluded.log,
  data_at = excluded.data_at,
  settings_at = excluded.settings_at,
  failures = 0
RETURNING id;

-- name: SaveRenderFailure :exec
-- Records a failed build, keeping the last good images and their time.
-- failures is 1 for a failure that counts toward the backoff and 0 for one
-- that does not, such as an unreachable renderer. It is added to the row's
-- count.
INSERT INTO render (instance_id, device_id, width, height, palette, time_zone, locale, rendered_at, attempted_at, duration_ms, error, log, data_at, settings_at, failures)
VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (instance_id, device_id, width, height, palette, time_zone, locale) DO UPDATE SET
  attempted_at = excluded.attempted_at,
  duration_ms = excluded.duration_ms,
  error = excluded.error,
  log = excluded.log,
  data_at = excluded.data_at,
  settings_at = excluded.settings_at,
  failures = render.failures + excluded.failures;

-- name: SaveRenderImage :exec
INSERT INTO render_image (render_id, layout, image, bytes)
VALUES (?, ?, ?, ?)
ON CONFLICT (render_id, layout) DO UPDATE SET image = excluded.image, bytes = excluded.bytes;

-- name: GetItemScreen :one
-- What a device showing an instance gets: the instance's name and refresh
-- interval, and the render for the device's size, palette, time zone and
-- locale, if any. A recipe's renders are either shared (device 0) or all per
-- device, so the device's own render is the one that matches, and the shared
-- one is the fallback.
SELECT plugin_instance.name, recipe.refresh_interval,
  render.id AS render_id, render.rendered_at, render.attempted_at, render.error,
  render_image.image, render_image.bytes
FROM plugin_instance
JOIN recipe ON recipe.id = plugin_instance.recipe_id
LEFT JOIN render ON render.instance_id = plugin_instance.id
  AND render.device_id IN (0, sqlc.arg(device_id))
  AND render.width = sqlc.arg(width) AND render.height = sqlc.arg(height) AND render.palette = sqlc.arg(palette)
  AND render.time_zone = sqlc.arg(time_zone) AND render.locale = sqlc.arg(locale)
LEFT JOIN render_image ON render_image.render_id = render.id AND render_image.layout = sqlc.arg(layout)
WHERE plugin_instance.id = sqlc.arg(instance_id)
ORDER BY render.device_id DESC
LIMIT 1;

-- name: ListRenders :many
-- Every render target's status, without the build log. device is the
-- friendly ID of the device a per-device render is for, NULL for a shared
-- render.
SELECT render.id, render.instance_id, device.friendly_id AS device, render.width, render.height, render.palette, render.time_zone, render.locale,
  render.rendered_at, render.attempted_at, render.duration_ms, render.error
FROM render
LEFT JOIN device ON device.id = render.device_id
ORDER BY render.instance_id, render.device_id, render.width, render.height, render.palette, render.time_zone, render.locale;

-- name: GetRenderStatus :one
-- One render target's status with its build log.
SELECT render.id, render.instance_id, device.friendly_id AS device, render.width, render.height, render.palette, render.time_zone, render.locale,
  render.rendered_at, render.attempted_at, render.duration_ms, render.error, render.log
FROM render
LEFT JOIN device ON device.id = render.device_id
WHERE render.id = ?;

-- name: ListRenderKeys :many
-- Every render's target, for finding the ones no device needs.
SELECT id, instance_id, device_id, width, height, palette, time_zone, locale FROM render;

-- name: DeleteRender :exec
-- Deletes a render and, by cascade, its images, which image expiry then
-- deletes once nothing else names them.
DELETE FROM render WHERE id = ?;

-- name: DeleteRenderWithImage :exec
-- Deletes a render whose image is missing from the image store, so the
-- scheduler builds it again. It does so only while the render still names
-- the image, so a build that replaced it in the meantime is kept.
DELETE FROM render
WHERE id = sqlc.arg(id)
  AND EXISTS (SELECT 1 FROM render_image WHERE render_image.render_id = render.id AND render_image.image = sqlc.arg(image));
