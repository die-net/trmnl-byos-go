package playlist

import (
	"context"
	"errors"

	"github.com/die-net/trmnl-byos-go/internal/db"
)

// addItemTo adds an item to the playlist with the name, which is made if it is
// missing, as the old AddItem did, and is the device's friendly ID when name is
// empty.
func addItemTo(ctx context.Context, client *db.Client, name, deviceName string, instanceID int64) (db.PlaylistItem, error) {
	if name == "" {
		name = deviceName
	}
	if _, err := CreatePlaylist(ctx, client, name); err != nil && !errors.Is(err, ErrNameTaken) {
		return db.PlaylistItem{}, err
	}
	return AddItem(ctx, client, ByName(name), deviceName, instanceID, DefaultDuration)
}
