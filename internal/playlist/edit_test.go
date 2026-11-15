package playlist

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// newPlaylist returns a database with a playlist named "p" of n items, whose
// IDs it returns in position order, and a second playlist "q" with one.
func newPlaylist(t *testing.T, n int) (*db.Client, []int64, int64) {
	t.Helper()
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	rec, err := client.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "r", Source: "s", RefreshInterval: 900, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := client.Write.CreateInstance(ctx, db.CreateInstanceParams{RecipeID: rec.ID, Name: "i", FieldValues: "{}", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, n)
	for range n {
		item, err := addItemTo(ctx, client, "p", "", inst.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.ID)
	}
	if _, err := addItemTo(ctx, client, "q", "", inst.ID); err != nil {
		t.Fatal(err)
	}
	return client, ids, inst.ID
}

// order returns the IDs of a playlist's items in position order, and fails if
// their positions are not 0 to n-1.
func order(t *testing.T, client *db.Client, name string) []int64 {
	t.Helper()
	ctx := t.Context()
	pl, err := client.Read.GetPlaylistByName(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := client.Read.ListPlaylistItemIDs(ctx, pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		item, err := client.Read.GetPlaylistItem(ctx, id)
		if err != nil || item.Position != int64(i) {
			t.Fatalf("item %d has position %d, %v; want %d", id, item.Position, err, i)
		}
	}
	return ids
}

func TestMoveItem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		item  int // index into the original order
		index int64
		want  []int // original indexes, in the new order
	}{
		{"down", 0, 2, []int{1, 2, 0, 3}},
		{"up", 3, 1, []int{0, 3, 1, 2}},
		{"to the front", 2, 0, []int{2, 0, 1, 3}},
		{"to the end", 1, 3, []int{0, 2, 3, 1}},
		{"in place", 1, 1, []int{0, 1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, ids, _ := newPlaylist(t, 4)
			if err := MoveItem(t.Context(), client, ids[tc.item], tc.index); err != nil {
				t.Fatal(err)
			}
			var want []int64
			for _, i := range tc.want {
				want = append(want, ids[i])
			}
			if got := order(t, client, "p"); !slices.Equal(got, want) {
				t.Errorf("order = %v, want %v", got, want)
			}
			// The other playlist is untouched.
			if got := order(t, client, "q"); len(got) != 1 {
				t.Errorf("q = %v", got)
			}
		})
	}
}

func TestMoveItemRefused(t *testing.T) {
	t.Parallel()
	client, ids, _ := newPlaylist(t, 3)
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		id    int64
		index int64
		want  error
	}{
		{"negative", ids[0], -1, errkind.ErrInvalid},
		{"past the end", ids[0], 3, errkind.ErrInvalid},
		{"far past the end", ids[0], 1 << 40, errkind.ErrInvalid},
		{"unknown item", ids[2] + 100, 0, ErrNoItem},
	} {
		if err := MoveItem(ctx, client, tc.id, tc.index); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if got := order(t, client, "p"); !slices.Equal(got, ids) {
			t.Errorf("%s changed the order to %v", tc.name, got)
		}
	}
}

func TestDeleteItemRenumbers(t *testing.T) {
	t.Parallel()
	client, ids, _ := newPlaylist(t, 4)
	ctx := t.Context()
	if err := DeleteItem(ctx, client, ids[1]); err != nil {
		t.Fatal(err)
	}
	if got, want := order(t, client, "p"), []int64{ids[0], ids[2], ids[3]}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if err := DeleteItem(ctx, client, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got, want := order(t, client, "p"), []int64{ids[2], ids[3]}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if got := order(t, client, "q"); len(got) != 1 {
		t.Errorf("q = %v", got)
	}
	// An item added after a delete takes the next index.
	item, err := addItemTo(ctx, client, "p", "", 1)
	if err != nil || item.Position != 2 {
		t.Errorf("AddItem = %+v, %v; want position 2", item, err)
	}
	order(t, client, "p")
}

func TestUpdateItem(t *testing.T) {
	t.Parallel()
	client, ids, _ := newPlaylist(t, 3)
	ctx := t.Context()
	yes, five, minutes := true, int64(5), 10*time.Minute
	days, _ := ParseDays("mon,fri")
	start, end := int64(600), int64(660)
	err := UpdateItem(ctx, client, ids[0], ItemUpdate{
		Position: &[]int64{2}[0],
		Duration: &minutes,
		Hidden:   &yes,
		Schedule: &ScheduleEdit{Days: &days, WindowStart: &start, WindowEnd: &end, StartsAt: &five},
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := client.Read.GetPlaylistItem(ctx, ids[0])
	want := db.PlaylistItem{ID: ids[0], PlaylistID: item.PlaylistID, Position: 2, Layout: "full", Duration: 600, Hidden: 1, Days: days, WindowStart: 600, WindowEnd: 660, StartsAt: 5}
	if err != nil || item != want {
		t.Errorf("item = %+v, %v; want %+v", item, err, want)
	}
	if got, w := order(t, client, "p"), []int64{ids[1], ids[2], ids[0]}; !slices.Equal(got, w) {
		t.Errorf("order = %v, want %v", got, w)
	}

	// A partial update leaves the rest as it was.
	no := false
	if err := UpdateItem(ctx, client, ids[0], ItemUpdate{Hidden: &no}); err != nil {
		t.Fatal(err)
	}
	item, err = client.Read.GetPlaylistItem(ctx, ids[0])
	want.Hidden = 0
	if err != nil || item != want {
		t.Errorf("after a partial update: %+v, %v; want %+v", item, err, want)
	}
}

func TestUpdateItemRefusedChangesNothing(t *testing.T) {
	t.Parallel()
	client, ids, _ := newPlaylist(t, 3)
	ctx := t.Context()
	before, err := client.Read.GetPlaylistItem(ctx, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	yes, short, ok := true, time.Second, 30*time.Minute
	zero, badStart, badWindow, two := int64(0), int64(1440), int64(60), int64(2)
	for _, tc := range []struct {
		name string
		id   int64
		u    ItemUpdate
		want error
	}{
		{"bad position with other changes", ids[1], ItemUpdate{Hidden: &yes, Duration: &ok, Position: &[]int64{3}[0]}, errkind.ErrInvalid},
		{"bad duration with other changes", ids[1], ItemUpdate{Hidden: &yes, Position: &two, Duration: &short}, errkind.ErrInvalid},
		{"no days", ids[1], ItemUpdate{Hidden: &yes, Position: &two, Schedule: &ScheduleEdit{Days: &zero}}, errkind.ErrInvalid},
		{"window out of range", ids[1], ItemUpdate{Hidden: &yes, Position: &two, Schedule: &ScheduleEdit{WindowStart: &badStart}}, errkind.ErrInvalid},
		{"backwards range", ids[1], ItemUpdate{Hidden: &yes, Schedule: &ScheduleEdit{StartsAt: &badWindow, EndsAt: &two}}, errkind.ErrInvalid},
		{"unknown item", ids[2] + 100, ItemUpdate{Hidden: &yes}, ErrNoItem},
	} {
		if err := UpdateItem(ctx, client, tc.id, tc.u); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		after, err := client.Read.GetPlaylistItem(ctx, ids[1])
		if err != nil || after != before {
			t.Errorf("%s changed the item: %+v, %v", tc.name, after, err)
		}
		if got := order(t, client, "p"); !slices.Equal(got, ids) {
			t.Errorf("%s changed the order to %v", tc.name, got)
		}
	}
}

func TestUpdatePlaylistSchedule(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	pl, err := client.Read.GetPlaylistByName(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	days, start, end, from, until := int64(5), int64(60), int64(30), int64(100), int64(200)
	err = UpdatePlaylist(ctx, client, ByID(pl.ID), Update{Schedule: &ScheduleEdit{Days: &days, WindowStart: &start, WindowEnd: &end, StartsAt: &from, EndsAt: &until}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Read.GetPlaylist(ctx, pl.ID)
	if want := (db.Playlist{ID: pl.ID, Name: "p", Days: 5, WindowStart: 60, WindowEnd: 30, StartsAt: 100, EndsAt: 200}); err != nil || got != want {
		t.Fatalf("playlist = %+v, %v; want %+v", got, err, want)
	}
	// A partial edit keeps the rest, and the result is what is validated: an
	// end date before the kept start date is refused.
	early := int64(50)
	if err := UpdatePlaylist(ctx, client, ByID(pl.ID), Update{Schedule: &ScheduleEdit{EndsAt: &early}}); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("end before the kept start: %v", err)
	}
	zero := int64(0)
	if err := UpdatePlaylist(ctx, client, ByID(pl.ID), Update{Schedule: &ScheduleEdit{Days: &zero}}); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("no days: %v", err)
	}
	if again, err := client.Read.GetPlaylist(ctx, pl.ID); err != nil || again != got {
		t.Errorf("a refused edit changed the playlist: %+v, %v", again, err)
	}
	if err := UpdatePlaylist(ctx, client, ByID(pl.ID+100), Update{}); !errors.Is(err, ErrNoPlaylist) {
		t.Errorf("unknown playlist: %v", err)
	}
}

func TestDeviceLists(t *testing.T) {
	t.Parallel()
	client, ids, instID := newPlaylist(t, 2)
	ctx := t.Context()
	dev, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: "D8:3B:DA:12:34:56", ApiKeyHash: []byte{1}, FriendlyID: "ABC123", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	// "q" has priority 0 and "p" 1, so q is tried first even though p has the lower ID.
	p, _ := client.Read.GetPlaylistByName(ctx, "p")
	q, _ := client.Read.GetPlaylistByName(ctx, "q")
	for _, id := range []int64{p.ID, q.ID} {
		if err := LinkDevice(ctx, client, ByID(id), dev.FriendlyID); err != nil {
			t.Fatal(err)
		}
	}
	if err := MoveLink(ctx, client, ByID(q.ID), dev.FriendlyID, []int64{p.ID, q.ID}, 0); err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := UpdateItem(ctx, client, ids[0], ItemUpdate{Hidden: &yes}); err != nil {
		t.Fatal(err)
	}
	rows, err := ListDeviceItems(ctx, client.Read, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	lists, groups := DeviceLists(rows)
	if len(lists) != 2 || lists[0].ID != q.ID || lists[1].ID != p.ID || len(groups[0]) != 1 || len(groups[1]) != 2 {
		t.Fatalf("lists = %+v, groups %d", lists, len(groups))
	}
	got := fmt.Sprint(lists[1].Entries[0].ID, lists[1].Entries[1].ID, lists[1].Entries[0].Hidden, lists[1].Entries[1].Hidden, groups[1][0].InstanceID == instID)
	if want := fmt.Sprint(ids[0], ids[1], true, false, true); got != want {
		t.Errorf("entries = %s, want %s", got, want)
	}
	if lists[1].Entries[0].Duration != DefaultDuration || lists[1].Schedule != Always {
		t.Errorf("duration or schedule: %+v", lists[1])
	}
}
