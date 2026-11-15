package deviceapi

import (
	"context"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
)

// addItemTo adds an item to the playlist with the name, which is made if it is
// missing, and is the device's friendly ID when name is
// empty.
func addItemTo(ctx context.Context, client *db.Client, name, deviceName string, instanceID int64, duration time.Duration) error {
	if name == "" {
		name = deviceName
	}
	_, err := playlist.AddItemCreating(ctx, client, name, deviceName, instanceID, duration)
	return err
}
