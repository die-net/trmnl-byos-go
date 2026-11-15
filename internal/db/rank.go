package db

import (
	"context"
)

// The queries in this file renumber rows with UPDATE ... FROM, which sqlc's
// SQLite parser does not accept (it also cannot resolve a CTE in an UPDATE),
// so they are constant strings with ? parameters, run on a Queries as the
// generated ones are (CLAUDE.md). Each takes two statements however many rows
// there are, with the negate query that goes before it. After that query every
// value is negative and distinct, and the old index is -1 - value. The new
// indexes are not negative, so no row meets another at any step, though SQLite
// checks a unique index one row at a time. The sort key of a row is twice its
// old index, which leaves room for the key of a moved row, 2*to+1 or 2*to-1.

const rankPlaylistItems = `UPDATE playlist_item SET position = ranked.idx
FROM (SELECT id, ROW_NUMBER() OVER (ORDER BY CASE WHEN id = ? THEN ? ELSE 2 * (-1 - position) END) - 1 AS idx
      FROM playlist_item WHERE playlist_id = ?) AS ranked
WHERE playlist_item.id = ranked.id`

// RankPlaylistItemsParams are the parameters of RankPlaylistItems.
type RankPlaylistItemsParams struct {
	// PlaylistID is the playlist whose items are renumbered.
	PlaylistID int64
	// MovedID is the item that is placed by MovedKey. 0 matches no item.
	MovedID int64
	// MovedKey is the sort key of the moved item.
	MovedKey int64
}

// RankPlaylistItems gives a playlist's items the positions 0 to n-1 in the
// order of their keys, after NegatePlaylistPositions. The key of an item is
// twice its old index, except the moved item's.
func (q *Queries) RankPlaylistItems(ctx context.Context, arg RankPlaylistItemsParams) error {
	_, err := q.db.ExecContext(ctx, rankPlaylistItems, arg.MovedID, arg.MovedKey, arg.PlaylistID)
	return err
}

const rankDeviceLinks = `UPDATE playlist_device SET priority = ranked.idx
FROM (SELECT playlist_id, ROW_NUMBER() OVER (ORDER BY CASE WHEN playlist_id = ? THEN ? ELSE 2 * (-1 - priority) END) - 1 AS idx
      FROM playlist_device WHERE device_id = ?) AS ranked
WHERE playlist_device.device_id = ? AND playlist_device.playlist_id = ranked.playlist_id`

// RankDeviceLinksParams are the parameters of RankDeviceLinks.
type RankDeviceLinksParams struct {
	// DeviceID is the device whose links are renumbered.
	DeviceID int64
	// MovedPlaylistID is the playlist whose link is placed by MovedKey. 0
	// matches no link.
	MovedPlaylistID int64
	// MovedKey is the sort key of the moved link.
	MovedKey int64
}

// RankDeviceLinks gives a device's links the priorities 0 to n-1 in the
// order of their keys, after NegateDevicePriorities. The key of a link is
// twice its old index, except the moved link's.
func (q *Queries) RankDeviceLinks(ctx context.Context, arg RankDeviceLinksParams) error {
	_, err := q.db.ExecContext(ctx, rankDeviceLinks, arg.MovedPlaylistID, arg.MovedKey, arg.DeviceID, arg.DeviceID)
	return err
}

const rankPlaylistDevicesWithout = `UPDATE playlist_device SET priority = ranked.idx
FROM (SELECT device_id, playlist_id, ROW_NUMBER() OVER (PARTITION BY device_id ORDER BY priority DESC) - 1 AS idx
      FROM playlist_device
      WHERE playlist_id <> ?
        AND device_id IN (SELECT other.device_id FROM playlist_device AS other WHERE other.playlist_id = ?)) AS ranked
WHERE playlist_device.device_id = ranked.device_id AND playlist_device.playlist_id = ranked.playlist_id`

// RankPlaylistDevicesWithout gives the links of every device linked to a
// playlist, other than the links to that playlist, the priorities 0 to n-1 in
// their old order, after NegatePlaylistDevicesPriorities. The links to the
// playlist keep their negative values until it is deleted.
func (q *Queries) RankPlaylistDevicesWithout(ctx context.Context, playlistID int64) error {
	_, err := q.db.ExecContext(ctx, rankPlaylistDevicesWithout, playlistID, playlistID)
	return err
}
