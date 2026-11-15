package playlist

import (
	"context"

	"github.com/die-net/trmnl-byos-go/internal/db"
)

// addItemTo adds an item to the playlist with the name, which is made if it is
// missing, and is the device's friendly ID when name is
// empty.
func addItemTo(ctx context.Context, client *db.Client, name, deviceName string, instanceID int64) (db.PlaylistItem, error) {
	if name == "" {
		name = deviceName
	}
	return AddItemCreating(ctx, client, name, deviceName, instanceID, DefaultDuration)
}
