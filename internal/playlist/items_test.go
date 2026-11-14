package playlist

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

func TestAddItem(t *testing.T) {
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	dev, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: "D8:3B:DA:12:34:56", ApiKeyHash: []byte{1}, FriendlyID: "ABC123", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := client.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "r", Source: "s", RefreshInterval: 900, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := client.Write.CreateInstance(ctx, db.CreateInstanceParams{RecipeID: rec.ID, Name: "i", FieldValues: "{}", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		playlist string
		device   int64
		instance int64
		duration time.Duration
		want     error
	}{
		{"unknown instance", "p", 0, inst.ID + 1, time.Hour, errkind.ErrNotFound},
		{"unknown instance for a device", "", dev.ID, inst.ID + 1, time.Hour, errkind.ErrNotFound},
		{"no playlist and no device", "", 0, inst.ID, time.Hour, errkind.ErrInvalid},
		{"too short", "p", 0, inst.ID, time.Second, errkind.ErrInvalid},
	} {
		if _, _, err := AddItem(ctx, client, tc.playlist, tc.device, tc.instance, tc.duration); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if lists, err := client.Read.ListPlaylists(ctx); err != nil || len(lists) != 0 {
		t.Fatalf("a refused item made a playlist: %v, %v", lists, err)
	}

	// With no name, the playlist is the device's friendly ID.
	item, name, err := AddItem(ctx, client, "", dev.ID, inst.ID, DefaultDuration)
	if err != nil || name != "ABC123" || item.Position != 0 || item.Duration != 900 {
		t.Fatalf("AddItem = %+v, %q, %v", item, name, err)
	}
	devs, err := client.Read.ListPlaylistDevices(ctx)
	if err != nil || len(devs) != 1 || devs[0].Playlist != "ABC123" {
		t.Errorf("playlist devices = %+v, %v", devs, err)
	}

	if err := DeleteItem(ctx, client, item.ID+1); !errors.Is(err, ErrNoItem) {
		t.Errorf("unknown item: %v", err)
	}
	if err := DeleteItem(ctx, client, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := DeleteItem(ctx, client, item.ID); !errors.Is(err, ErrNoItem) {
		t.Errorf("item deleted twice: %v", err)
	}
}
