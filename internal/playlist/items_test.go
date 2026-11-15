package playlist

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

func TestAddItem(t *testing.T) {
	t.Parallel()
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
		playlist Ref
		device   string
		instance int64
		duration time.Duration
		want     error
	}{
		{"unknown instance", ByName("p"), "", inst.ID + 1, time.Hour, errkind.ErrNotFound},
		{"unknown instance for a device", ByName("ABC123"), dev.FriendlyID, inst.ID + 1, time.Hour, errkind.ErrNotFound},
		{"missing playlist by name", ByName("p"), "", inst.ID, time.Hour, ErrNoPlaylist},
		{"missing playlist by ID", ByID(99), dev.FriendlyID, inst.ID, time.Hour, ErrNoPlaylist},
		{"missing playlist for a device", ByName("ABC123"), dev.FriendlyID, inst.ID, time.Hour, ErrNoPlaylist},
	} {
		if _, err := AddItem(ctx, client, tc.playlist, tc.device, tc.instance, tc.duration); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if lists, err := client.Read.ListPlaylists(ctx); err != nil || len(lists) != 0 {
		t.Fatalf("a refused item made a playlist: %v, %v", lists, err)
	}

	pl, err := CreatePlaylist(ctx, client, "ABC123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddItem(ctx, client, ByID(pl.ID), "ZZZ999", inst.ID, DefaultDuration); !errors.Is(err, device.ErrNoDevice) {
		t.Errorf("unknown device: %v", err)
	}
	item, err := AddItem(ctx, client, ByID(pl.ID), dev.FriendlyID, inst.ID, DefaultDuration)
	if err != nil || item.PlaylistID != pl.ID || item.Position != 0 || item.Duration != 900 {
		t.Fatalf("AddItem = %+v, %v", item, err)
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
