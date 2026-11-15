package adminui

import (
	"context"
	"errors"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
)

// addItemTo adds an item to the playlist with the name, which is made if it is
// missing, as the old AddItem did, and is the device's friendly ID when name is
// empty.
func addItemTo(ctx context.Context, client *db.Client, name, deviceName string, instanceID int64, duration time.Duration) (db.PlaylistItem, error) {
	if name == "" {
		name = deviceName
	}
	if _, err := playlist.CreatePlaylist(ctx, client, name); err != nil && !errors.Is(err, playlist.ErrNameTaken) {
		return db.PlaylistItem{}, err
	}
	return playlist.AddItem(ctx, client, playlist.ByName(name), deviceName, instanceID, duration)
}
