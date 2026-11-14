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

-- name: ListDeviceItems :many
-- The items of the playlist a device shows, in order, with the instance in
-- each item's first slot.
SELECT playlist_item.id, playlist_item.duration, playlist_slot.instance_id, plugin_instance.palette
FROM playlist_item
JOIN playlist_slot ON playlist_slot.item_id = playlist_item.id AND playlist_slot.slot = 0
JOIN plugin_instance ON plugin_instance.id = playlist_slot.instance_id
WHERE playlist_item.playlist_id = (
  SELECT playlist_device.playlist_id FROM playlist_device
  WHERE playlist_device.device_id = ?
  ORDER BY playlist_device.priority, playlist_device.playlist_id
  LIMIT 1
)
ORDER BY playlist_item.position;

-- name: ListPlaylistItems :many
-- Every playlist's items, for listing.
SELECT playlist.name AS playlist, playlist_item.id, playlist_item.position, playlist_item.duration,
  plugin_instance.id AS instance_id, plugin_instance.name AS instance_name
FROM playlist
JOIN playlist_item ON playlist_item.playlist_id = playlist.id
JOIN playlist_slot ON playlist_slot.item_id = playlist_item.id AND playlist_slot.slot = 0
JOIN plugin_instance ON plugin_instance.id = playlist_slot.instance_id
ORDER BY playlist.name, playlist_item.position;

-- name: ListPlaylistDevices :many
SELECT playlist.name AS playlist, device.friendly_id, playlist_device.priority
FROM playlist_device
JOIN playlist ON playlist.id = playlist_device.playlist_id
JOIN device ON device.id = playlist_device.device_id
ORDER BY playlist.name, device.friendly_id;

-- name: ListInstanceItems :many
-- The playlist items that show an instance in any slot.
SELECT DISTINCT item_id FROM playlist_slot WHERE instance_id = ? ORDER BY item_id;

-- name: ListPlaylists :many
SELECT * FROM playlist ORDER BY name;
