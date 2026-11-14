-- name: ListReferencedImages :many
-- Every image name a row refers to, which image expiry must keep.
SELECT last_image AS image FROM device_state WHERE last_image != ''
UNION
SELECT image FROM render_image;
