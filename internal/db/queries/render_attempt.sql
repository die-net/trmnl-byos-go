-- name: StartRenderAttempt :exec
-- Records that a build of a target is starting. A row left by an earlier
-- build of the same target, which the scheduler would have converted at
-- startup, is replaced.
INSERT INTO render_attempt (instance_id, device_id, width, height, palette, time_zone, locale, started_at, data_at, settings_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (instance_id, device_id, width, height, palette, time_zone, locale) DO UPDATE SET
  started_at = excluded.started_at,
  data_at = excluded.data_at,
  settings_at = excluded.settings_at;

-- name: EndRenderAttempt :exec
-- Removes a target's record of a build once its outcome is saved, or the
-- build ended with nothing to record.
DELETE FROM render_attempt
WHERE instance_id = ? AND device_id = ? AND width = ? AND height = ? AND palette = ? AND time_zone = ? AND locale = ?;

-- name: ListRenderAttempts :many
-- Every build that started and recorded no outcome, which at startup means
-- the server stopped during it.
SELECT instance_id, device_id, width, height, palette, time_zone, locale, started_at, data_at, settings_at
FROM render_attempt
ORDER BY instance_id, device_id, width, height, palette, time_zone, locale;
