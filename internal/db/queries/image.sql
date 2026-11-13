-- name: ListReferencedImages :many
-- Every image name a row refers to, which image expiry must keep.
SELECT DISTINCT last_image FROM device_state WHERE last_image != '';
