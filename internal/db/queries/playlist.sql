-- name: GetPlaylistByName :one
SELECT * FROM playlist WHERE name = ?;

-- name: CreatePlaylist :one
INSERT INTO playlist (name) VALUES (?) RETURNING *;

-- name: LinkPlaylistDevice :exec
INSERT INTO playlist_device (device_id, playlist_id, priority)
VALUES (?, ?, ?)
ON CONFLICT (device_id, playlist_id) DO UPDATE SET priority = excluded.priority;

-- name: NextItemPosition :one
SELECT CAST(COALESCE(MAX(position), -1) + 1 AS INTEGER) FROM playlist_item WHERE playlist_id = ?;

-- name: CreatePlaylistItem :one
INSERT INTO playlist_item (playlist_id, position, layout, duration)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: CreatePlaylistSlot :exec
INSERT INTO playlist_slot (item_id, slot, instance_id) VALUES (?, ?, ?);

-- name: DeletePlaylistItem :execrows
DELETE FROM playlist_item WHERE id = ?;

-- name: GetPlaylistItem :one
SELECT * FROM playlist_item WHERE id = ?;

-- name: ListPlaylistItemIDs :many
-- A playlist's item IDs in position order.
SELECT id FROM playlist_item WHERE playlist_id = ? ORDER BY position;

-- name: NegatePlaylistPositions :exec
-- Moves a playlist's positions out of the range of indexes, so each item can
-- then be given its index without two items sharing a position at any step.
UPDATE playlist_item SET position = -1 - position WHERE playlist_id = ?;

-- name: SetItemPosition :exec
UPDATE playlist_item SET position = ? WHERE id = ?;

-- name: UpdatePlaylistItem :exec
UPDATE playlist_item
SET duration = sqlc.arg(duration), hidden = sqlc.arg(hidden), days = sqlc.arg(days),
  window_start = sqlc.arg(window_start), window_end = sqlc.arg(window_end),
  starts_at = sqlc.arg(starts_at), ends_at = sqlc.arg(ends_at)
WHERE id = sqlc.arg(id);

-- name: GetPlaylist :one
SELECT * FROM playlist WHERE id = ?;

-- name: UpdatePlaylistSchedule :exec
UPDATE playlist
SET days = ?, window_start = ?, window_end = ?, starts_at = ?, ends_at = ?
WHERE id = ?;

-- name: ListDeviceItems :many
-- The items of every playlist a device is linked to, in the order they are
-- tried (priority, then playlist ID) and then position, with each
-- playlist's and item's schedule and the instance in each item's first slot.
SELECT playlist.id AS playlist_id, playlist.days AS playlist_days, playlist.window_start AS playlist_window_start,
  playlist.window_end AS playlist_window_end, playlist.starts_at AS playlist_starts_at, playlist.ends_at AS playlist_ends_at,
  playlist_item.id, playlist_item.duration, playlist_item.hidden, playlist_item.days AS item_days,
  playlist_item.window_start AS item_window_start, playlist_item.window_end AS item_window_end,
  playlist_item.starts_at AS item_starts_at, playlist_item.ends_at AS item_ends_at,
  playlist_slot.instance_id, plugin_instance.palette, plugin_instance.name AS instance_name
FROM playlist_device
JOIN playlist ON playlist.id = playlist_device.playlist_id
JOIN playlist_item ON playlist_item.playlist_id = playlist.id
JOIN playlist_slot ON playlist_slot.item_id = playlist_item.id AND playlist_slot.slot = 0
JOIN plugin_instance ON plugin_instance.id = playlist_slot.instance_id
WHERE playlist_device.device_id = ?
ORDER BY playlist_device.priority, playlist_device.playlist_id, playlist_item.position;

-- name: ListPlaylistItems :many
-- Every playlist's items, for listing.
SELECT playlist.name AS playlist, playlist_item.id, playlist_item.position, playlist_item.duration, playlist_item.hidden,
  playlist_item.days, playlist_item.window_start, playlist_item.window_end, playlist_item.starts_at, playlist_item.ends_at,
  plugin_instance.id AS instance_id, plugin_instance.name AS instance_name
FROM playlist
JOIN playlist_item ON playlist_item.playlist_id = playlist.id
JOIN playlist_slot ON playlist_slot.item_id = playlist_item.id AND playlist_slot.slot = 0
JOIN plugin_instance ON plugin_instance.id = playlist_slot.instance_id
ORDER BY playlist.name, playlist_item.position;

-- name: GetPlaylistItemView :one
-- One item with its playlist's name and the instance in its first slot.
SELECT sqlc.embed(playlist_item), playlist.name AS playlist_name, plugin_instance.name AS instance_name
FROM playlist_item
JOIN playlist ON playlist.id = playlist_item.playlist_id
JOIN playlist_slot ON playlist_slot.item_id = playlist_item.id AND playlist_slot.slot = 0
JOIN plugin_instance ON plugin_instance.id = playlist_slot.instance_id
WHERE playlist_item.id = ?;

-- name: ListPlaylistDevices :many
SELECT playlist.name AS playlist, device.friendly_id, device.name AS device_name, playlist_device.priority
FROM playlist_device
JOIN playlist ON playlist.id = playlist_device.playlist_id
JOIN device ON device.id = playlist_device.device_id
ORDER BY playlist.name, device.friendly_id;

-- name: ListInstanceItems :many
-- The playlist items that show an instance in any slot.
SELECT DISTINCT item_id FROM playlist_slot WHERE instance_id = ? ORDER BY item_id;

-- name: ListPlaylists :many
SELECT * FROM playlist ORDER BY name;
