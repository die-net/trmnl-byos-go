package playlist

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// newDevice adds a device with a made-up MAC and friendly ID from n.
func newDevice(t *testing.T, client *db.Client, n int) db.Device {
	t.Helper()
	dev, err := client.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
		Mac: fmt.Sprintf("D8:3B:DA:12:34:%02X", n), ApiKeyHash: []byte(fmt.Sprintf("key %d", n)), FriendlyID: fmt.Sprintf("ABC%03d", n), CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dev
}

// newNamed adds empty playlists with the names and returns their IDs.
func newNamed(t *testing.T, client *db.Client, names ...string) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(names))
	for _, n := range names {
		pl, err := CreatePlaylist(t.Context(), client, n)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, pl.ID)
	}
	return ids
}

// links returns the IDs of the playlists a device is linked to in priority
// order, and fails if the priorities are not 0 to n-1.
func links(t *testing.T, client *db.Client, deviceID int64) []int64 {
	t.Helper()
	rows, err := client.Read.ListPlaylistDevices(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var mine []db.ListPlaylistDevicesRow
	for _, r := range rows {
		if r.DeviceID == deviceID {
			mine = append(mine, r)
		}
	}
	slices.SortFunc(mine, func(a, b db.ListPlaylistDevicesRow) int { return int(a.Priority - b.Priority) })
	ids := make([]int64, len(mine))
	for i, r := range mine {
		if r.Priority != int64(i) {
			t.Fatalf("device %d: link to %d has priority %d, want %d", deviceID, r.PlaylistID, r.Priority, i)
		}
		ids[i] = r.PlaylistID
	}
	return ids
}

func TestCleanName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in, want string
		invalid        bool
	}{
		{"plain", "Kitchen", "Kitchen", false},
		{"trimmed", "  Kitchen \t", "Kitchen", false},
		{"empty", "", "", true},
		{"blank", " \t ", "", true},
		{"too long", strings.Repeat("x", device.MaxNameBytes+1), "", true},
		{"longest", strings.Repeat("x", device.MaxNameBytes), strings.Repeat("x", device.MaxNameBytes), false},
		{"control character", "a\nb", "", true},
		{"not UTF-8", "a\xffb", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := CleanName(tc.in)
			if tc.invalid != errors.Is(err, errkind.ErrInvalid) || got != tc.want {
				t.Errorf("CleanName(%q) = %q, %v; want %q, invalid %v", tc.in, got, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestCreatePlaylist(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	pl, err := CreatePlaylist(ctx, client, "  Mornings ")
	if err != nil || pl.Name != "Mornings" || pl.Days != int64(AllDays) {
		t.Fatalf("created %+v, %v", pl, err)
	}
	for _, tc := range []struct {
		name, in string
		kind     error
	}{
		{"duplicate", "Mornings", ErrNameTaken},
		{"duplicate after trimming", " Mornings", ErrNameTaken},
		{"taken by an item playlist", "p", ErrNameTaken},
		{"empty", "", errkind.ErrInvalid},
		{"too long", strings.Repeat("x", 65), errkind.ErrInvalid},
		{"control character", "a\x00b", errkind.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := CreatePlaylist(ctx, client, tc.in); !errors.Is(err, tc.kind) {
				t.Errorf("CreatePlaylist(%q) = %v, want %v", tc.in, err, tc.kind)
			}
		})
	}
	if !errors.Is(ErrNameTaken, errkind.ErrConflict) {
		t.Error("ErrNameTaken is not a conflict")
	}
}

func TestUpdatePlaylistName(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	p, _ := client.Read.GetPlaylistByName(ctx, "p")
	q, _ := client.Read.GetPlaylistByName(ctx, "q")
	name := func(s string) Update { return Update{Name: &s} }
	for _, tc := range []struct {
		name string
		id   int64
		u    Update
		kind error
	}{
		{"to another's name", p.ID, name("q"), ErrNameTaken},
		{"empty", p.ID, name(""), errkind.ErrInvalid},
		{"too long", p.ID, name(strings.Repeat("x", 65)), errkind.ErrInvalid},
		{"missing", q.ID + 100, name("z"), ErrNoPlaylist},
	} {
		if err := UpdatePlaylist(ctx, client, ByID(tc.id), tc.u); !errors.Is(err, tc.kind) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.kind)
		}
	}
	if got, err := client.Read.GetPlaylist(ctx, p.ID); err != nil || got != p {
		t.Errorf("a refused rename changed the playlist: %+v, %v", got, err)
	}
	// Its own name, with space around it, is fine, and so is a new one.
	if err := UpdatePlaylist(ctx, client, ByID(p.ID), name(" p ")); err != nil {
		t.Errorf("rename to its own name: %v", err)
	}
	if err := UpdatePlaylist(ctx, client, ByID(p.ID), name("renamed")); err != nil {
		t.Fatal(err)
	}
	if got, err := client.Read.GetPlaylist(ctx, p.ID); err != nil || got.Name != "renamed" {
		t.Errorf("after rename: %+v, %v", got, err)
	}
	// The old name is free again.
	if _, err := CreatePlaylist(ctx, client, "p"); err != nil {
		t.Errorf("create with the freed name: %v", err)
	}
}

// TestUpdatePlaylistAtomic checks that a schedule that is refused stops the
// rename that came with it, and a name that is refused stops the schedule.
func TestUpdatePlaylistAtomic(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	p, _ := client.Read.GetPlaylistByName(ctx, "p")
	zero, other, fresh := int64(0), "q", "fresh"
	days := int64(5)
	for _, tc := range []struct {
		name string
		u    Update
		kind error
	}{
		{"bad schedule", Update{Name: &fresh, Schedule: &ScheduleEdit{Days: &zero}}, errkind.ErrInvalid},
		{"taken name", Update{Name: &other, Schedule: &ScheduleEdit{Days: &days}}, ErrNameTaken},
	} {
		if err := UpdatePlaylist(ctx, client, ByID(p.ID), tc.u); !errors.Is(err, tc.kind) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.kind)
		}
		if got, err := client.Read.GetPlaylist(ctx, p.ID); err != nil || got != p {
			t.Errorf("%s: playlist changed: %+v, %v", tc.name, got, err)
		}
	}
	// Both parts together apply.
	if err := UpdatePlaylist(ctx, client, ByID(p.ID), Update{Name: &fresh, Schedule: &ScheduleEdit{Days: &days}}); err != nil {
		t.Fatal(err)
	}
	if got, err := client.Read.GetPlaylist(ctx, p.ID); err != nil || got.Name != "fresh" || got.Days != 5 {
		t.Errorf("after update: %+v, %v", got, err)
	}
}

// seenOf returns the name and schedule the playlist has now, as a caller
// that read it would hold them.
func seenOf(t *testing.T, client *db.Client, id int64) Seen {
	t.Helper()
	pl, err := client.Read.GetPlaylist(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return Seen{Name: pl.Name, Schedule: Schedule{Days: pl.Days, WindowStart: pl.WindowStart, WindowEnd: pl.WindowEnd, StartsAt: pl.StartsAt, EndsAt: pl.EndsAt}}
}

// TestUpdatePlaylistSeen saves from a view that another change has made out
// of date, in both directions, and checks that nothing is written.
func TestUpdatePlaylistSeen(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	p, _ := client.Read.GetPlaylistByName(ctx, "p")
	saw := seenOf(t, client, p.ID)
	weekdays := Schedule{Days: 0b0111110, WindowStart: 540, WindowEnd: 1020, StartsAt: 86400, EndsAt: 172800}
	edit := weekdays.Full()
	mornings, breakfast := "Mornings", "Breakfast"
	// Another change renames the playlist, and then a save of days from the
	// older view is refused.
	if err := UpdatePlaylist(ctx, client, ByID(p.ID), Update{Name: &mornings}); err != nil {
		t.Fatal(err)
	}
	days := int64(0b0000110)
	err := UpdatePlaylist(ctx, client, ByID(p.ID), Update{Name: &saw.Name, Schedule: &ScheduleEdit{Days: &days}, Seen: &saw})
	if !errors.Is(err, ErrPlaylistChanged) || !errors.Is(err, errkind.ErrConflict) {
		t.Fatalf("save after a rename: %v", err)
	}
	if got := seenOf(t, client, p.ID); got.Name != mornings || got.Schedule != saw.Schedule {
		t.Errorf("a refused save changed the playlist: %+v", got)
	}
	// Another change sets the schedule, and then a rename from the older
	// view is refused.
	saw = seenOf(t, client, p.ID)
	if err := UpdatePlaylist(ctx, client, ByID(p.ID), Update{Schedule: &edit}); err != nil {
		t.Fatal(err)
	}
	err = UpdatePlaylist(ctx, client, ByID(p.ID), Update{Name: &breakfast, Schedule: &ScheduleEdit{Days: &days}, Seen: &saw})
	if !errors.Is(err, ErrPlaylistChanged) {
		t.Fatalf("save after a schedule change: %v", err)
	}
	if got := seenOf(t, client, p.ID); got.Name != mornings || got.Schedule != weekdays {
		t.Errorf("a refused save changed the playlist: %+v", got)
	}
	// A save from a view that matches goes through, and the view it is read
	// back as matches too, with every schedule field in the stored form.
	saw = seenOf(t, client, p.ID)
	if err := UpdatePlaylist(ctx, client, ByID(p.ID), Update{Name: &breakfast, Schedule: &ScheduleEdit{Days: &days}, Seen: &saw}); err != nil {
		t.Fatal(err)
	}
	saw = seenOf(t, client, p.ID)
	if err := UpdatePlaylist(ctx, client, ByID(p.ID), Update{Name: &mornings, Seen: &saw}); err != nil {
		t.Fatalf("save from a view read back: %v", err)
	}
	// No view means the last write wins.
	if err := UpdatePlaylist(ctx, client, ByID(p.ID), Update{Name: &breakfast, Schedule: &edit}); err != nil {
		t.Fatal(err)
	}
	if got := seenOf(t, client, p.ID); got.Name != breakfast || got.Schedule != weekdays {
		t.Errorf("after an unchecked save: %+v", got)
	}
}

// TestDeletePlaylistSeen deletes with a view of what goes with the playlist
// that is out of date by a device or an item, which refuses, and with one
// that matches, which deletes.
func TestDeletePlaylistSeen(t *testing.T) {
	t.Parallel()
	client, _, inst := newPlaylist(t, 1)
	ctx := t.Context()
	pl := newNamed(t, client, "empty")[0]
	d1, d2 := newDevice(t, client, 1), newDevice(t, client, 2)
	exists := func() bool {
		t.Helper()
		_, err := client.Read.GetPlaylist(ctx, pl)
		return err == nil
	}
	if err := LinkDevice(ctx, client, ByID(pl), d1.FriendlyID); err != nil {
		t.Fatal(err)
	}
	// The view had no devices, and one is linked now.
	if err := DeletePlaylist(ctx, client, ByID(pl), &Contents{Items: 0}); !errors.Is(err, ErrPlaylistChanged) || !errors.Is(err, errkind.ErrConflict) {
		t.Fatalf("delete after a link: %v", err)
	}
	// The view had a device the playlist has lost, and a different one.
	if err := DeletePlaylist(ctx, client, ByID(pl), &Contents{Devices: []int64{d2.ID}}); !errors.Is(err, ErrPlaylistChanged) {
		t.Errorf("delete with another device: %v", err)
	}
	if err := DeletePlaylist(ctx, client, ByID(pl), &Contents{Devices: []int64{d1.ID, d2.ID}}); !errors.Is(err, ErrPlaylistChanged) {
		t.Errorf("delete with an extra device: %v", err)
	}
	// The view had no items, and one is added.
	if _, err := AddItem(ctx, client, ByID(pl), "", inst, 0); err != nil {
		t.Fatal(err)
	}
	if err := DeletePlaylist(ctx, client, ByID(pl), &Contents{Items: 0, Devices: []int64{d1.ID}}); !errors.Is(err, ErrPlaylistChanged) {
		t.Errorf("delete after an item: %v", err)
	}
	if !exists() || !slices.Equal(links(t, client, d1.ID), []int64{pl}) {
		t.Fatal("a refused delete changed something")
	}
	// A view that matches deletes, whatever the order or repeats of devices.
	if err := DeletePlaylist(ctx, client, ByID(pl), &Contents{Items: 1, Devices: []int64{d1.ID, d1.ID}}); err != nil {
		t.Fatal(err)
	}
	if exists() {
		t.Error("the playlist is still there")
	}
}

func TestDeletePlaylist(t *testing.T) {
	t.Parallel()
	client, ids, _ := newPlaylist(t, 2)
	ctx := t.Context()
	pq := newNamed(t, client, "a", "b", "c")
	d1, d2 := newDevice(t, client, 1), newDevice(t, client, 2)
	// d1 is linked to a, b, c in that order, d2 to c, b.
	for _, l := range []struct {
		pl  int64
		dev string
	}{{pq[0], d1.FriendlyID}, {pq[1], d1.FriendlyID}, {pq[2], d1.FriendlyID}, {pq[2], d2.FriendlyID}, {pq[1], d2.FriendlyID}} {
		if err := LinkDevice(ctx, client, ByID(l.pl), l.dev); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeletePlaylist(ctx, client, ByID(pq[0]), nil); err != nil {
		t.Fatal(err)
	}
	if got := links(t, client, d1.ID); !slices.Equal(got, []int64{pq[1], pq[2]}) {
		t.Errorf("d1 links = %v", got)
	}
	if got := links(t, client, d2.ID); !slices.Equal(got, []int64{pq[2], pq[1]}) {
		t.Errorf("d2 links = %v", got)
	}
	// A playlist with items loses them, and a device on it is renumbered.
	p, _ := client.Read.GetPlaylistByName(ctx, "p")
	if err := LinkDevice(ctx, client, ByID(p.ID), d2.FriendlyID); err != nil {
		t.Fatal(err)
	}
	if err := DeletePlaylist(ctx, client, ByID(p.ID), nil); err != nil {
		t.Fatal(err)
	}
	if got := links(t, client, d2.ID); !slices.Equal(got, []int64{pq[2], pq[1]}) {
		t.Errorf("d2 links after deleting p = %v", got)
	}
	if _, err := client.Read.GetPlaylistItem(ctx, ids[0]); err == nil {
		t.Error("an item of the deleted playlist is still there")
	}
	if err := DeletePlaylist(ctx, client, ByID(p.ID), nil); !errors.Is(err, ErrNoPlaylist) {
		t.Errorf("delete again: %v", err)
	}
}

func TestLinkDevice(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	pl := newNamed(t, client, "a", "b", "c")
	dev := newDevice(t, client, 1)
	for _, id := range pl[:2] {
		if err := LinkDevice(ctx, client, ByID(id), dev.FriendlyID); err != nil {
			t.Fatal(err)
		}
	}
	// A new link goes last, and by MAC and by name works too.
	if err := LinkDevice(ctx, client, ByName("c"), dev.Mac); err != nil {
		t.Fatal(err)
	}
	if got := links(t, client, dev.ID); !slices.Equal(got, pl) {
		t.Errorf("links = %v, want %v", got, pl)
	}
	// Linking again leaves the link where it is.
	if err := MoveLink(ctx, client, ByID(pl[2]), dev.FriendlyID, pl, 0); err != nil {
		t.Fatal(err)
	}
	if err := LinkDevice(ctx, client, ByID(pl[2]), dev.FriendlyID); err != nil {
		t.Fatal(err)
	}
	if want := []int64{pl[2], pl[0], pl[1]}; !slices.Equal(links(t, client, dev.ID), want) {
		t.Errorf("links after relinking = %v, want %v", links(t, client, dev.ID), want)
	}
}

func TestMoveLink(t *testing.T) {
	t.Parallel()
	// Each case starts with a device linked to a, b, c, d in that order.
	for _, tc := range []struct {
		name     string
		playlist string
		index    int64
		want     string // playlist names in order
	}{
		{"to the first place", "d", 0, "dabc"},
		{"to the last place", "a", 3, "bcda"},
		{"down", "a", 2, "bcad"},
		{"up", "d", 1, "adbc"},
		{"to its own index", "b", 1, "abcd"},
		{"one place down", "b", 2, "acbd"},
		{"one place up", "c", 1, "acbd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _, _ := newPlaylist(t, 1)
			ctx := t.Context()
			byName := map[string]int64{}
			dev := newDevice(t, client, 1)
			for _, n := range []string{"a", "b", "c", "d"} {
				byName[n] = newNamed(t, client, n)[0]
				if err := LinkDevice(ctx, client, ByID(byName[n]), dev.FriendlyID); err != nil {
					t.Fatal(err)
				}
			}
			if err := MoveLink(ctx, client, ByID(byName[tc.playlist]), dev.FriendlyID, links(t, client, dev.ID), tc.index); err != nil {
				t.Fatal(err)
			}
			var got strings.Builder
			for _, id := range links(t, client, dev.ID) {
				for n, v := range byName {
					if v == id {
						got.WriteString(n)
					}
				}
			}
			if got.String() != tc.want {
				t.Errorf("order = %s, want %s", got.String(), tc.want)
			}
		})
	}
}

func TestMoveLinkRefused(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	pl := newNamed(t, client, "a", "b", "c")
	dev, other := newDevice(t, client, 1), newDevice(t, client, 2)
	for _, id := range pl[:2] {
		if err := LinkDevice(ctx, client, ByID(id), dev.FriendlyID); err != nil {
			t.Fatal(err)
		}
	}
	a, b, c := pl[0], pl[1], pl[2]
	for _, tc := range []struct {
		name  string
		ref   Ref
		dev   string
		shown []int64
		to    int64
		kind  error
		text  string
	}{
		{"not linked", ByID(c), dev.FriendlyID, []int64{a, b}, 0, ErrNotLinked, "not linked"},
		{"other device not linked", ByID(a), other.FriendlyID, nil, 0, ErrNotLinked, ""},
		{"unknown playlist", ByID(999), dev.FriendlyID, []int64{a, b}, 0, ErrNoPlaylist, ""},
		{"unknown playlist name", ByName("nope"), dev.FriendlyID, []int64{a, b}, 0, ErrNoPlaylist, ""},
		{"unknown device", ByID(a), "ZZZ999", []int64{a, b}, 0, device.ErrNoDevice, ""},
		{"negative index", ByID(a), dev.FriendlyID, []int64{a, b}, -1, errkind.ErrInvalid, "-1 is not a place in the device's playlist order, which runs from 0 to 1"},
		{"index n", ByID(a), dev.FriendlyID, []int64{a, b}, 2, errkind.ErrInvalid, "2 is not a place in the device's playlist order, which runs from 0 to 1"},
		{"stale order, swapped", ByID(b), dev.FriendlyID, []int64{b, a}, 0, ErrOrderChanged, "the device's playlist order has changed: the order is now"},
		{"stale order, same place", ByID(a), dev.FriendlyID, []int64{b, a}, 1, ErrOrderChanged, ""},
		{"stale order, out of range", ByID(a), dev.FriendlyID, []int64{b, a}, 7, ErrOrderChanged, ""},
		{"stale order, a link since removed", ByID(a), dev.FriendlyID, []int64{a, b, c}, 1, ErrOrderChanged, ""},
		{"stale order, a link since added", ByID(a), dev.FriendlyID, []int64{a}, 0, ErrOrderChanged, ""},
		{"no order shown", ByID(a), dev.FriendlyID, nil, 1, ErrOrderChanged, ""},
	} {
		err := MoveLink(ctx, client, tc.ref, tc.dev, tc.shown, tc.to)
		if !errors.Is(err, tc.kind) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.kind)
		} else if !strings.Contains(err.Error(), tc.text) {
			t.Errorf("%s: message %q, want it to contain %q", tc.name, err, tc.text)
		}
	}
	if !errors.Is(ErrOrderChanged, errkind.ErrConflict) {
		t.Error("ErrOrderChanged is not a conflict")
	}
	// Nothing changed, and no link was made for the pairs that had none.
	if got := links(t, client, dev.ID); !slices.Equal(got, pl[:2]) {
		t.Errorf("links = %v, want %v", got, pl[:2])
	}
	if got := links(t, client, other.ID); len(got) != 0 {
		t.Errorf("a refused move made links: %v", got)
	}
}

// TestMoveLinkStaleOrder moves the last of three playlists up from a view
// taken before another change swapped the first two. The move is refused, and
// the order stays as the other change left it.
func TestMoveLinkStaleOrder(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	pl := newNamed(t, client, "a", "b", "c")
	dev := newDevice(t, client, 1)
	for _, id := range pl {
		if err := LinkDevice(ctx, client, ByID(id), dev.FriendlyID); err != nil {
			t.Fatal(err)
		}
	}
	shown := links(t, client, dev.ID)
	if err := MoveLink(ctx, client, ByID(pl[1]), dev.FriendlyID, shown, 0); err != nil {
		t.Fatal(err)
	}
	swapped := []int64{pl[1], pl[0], pl[2]}
	if err := MoveLink(ctx, client, ByID(pl[2]), dev.FriendlyID, shown, 1); !errors.Is(err, ErrOrderChanged) {
		t.Fatalf("move from a stale order: %v", err)
	}
	if got := links(t, client, dev.ID); !slices.Equal(got, swapped) {
		t.Errorf("links = %v, want %v", got, swapped)
	}
	// The same move from the current order goes through.
	if err := MoveLink(ctx, client, ByID(pl[2]), dev.FriendlyID, swapped, 1); err != nil {
		t.Fatal(err)
	}
	if got, want := links(t, client, dev.ID), []int64{pl[1], pl[2], pl[0]}; !slices.Equal(got, want) {
		t.Errorf("links = %v, want %v", got, want)
	}
}

func TestLinkDeviceRefused(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	p, _ := client.Read.GetPlaylistByName(ctx, "p")
	dev := newDevice(t, client, 1)
	if err := LinkDevice(ctx, client, ByID(p.ID+100), dev.FriendlyID); !errors.Is(err, ErrNoPlaylist) {
		t.Errorf("unknown playlist: %v", err)
	}
	if err := LinkDevice(ctx, client, ByID(p.ID), "ZZZ999"); !errors.Is(err, device.ErrNoDevice) {
		t.Errorf("unknown device: %v", err)
	}
	if err := LinkDevice(ctx, client, ByName("nope"), dev.FriendlyID); !errors.Is(err, ErrNoPlaylist) {
		t.Errorf("unknown playlist name: %v", err)
	}
	// A device by MAC and a playlist by name work as well.
	if err := LinkDevice(ctx, client, ByName("p"), dev.Mac); err != nil {
		t.Errorf("by name and MAC: %v", err)
	}
	if got := links(t, client, dev.ID); len(got) != 1 {
		t.Errorf("links = %v", got)
	}
	if err := UnlinkDevice(ctx, client, ByName("p"), dev.Mac); err != nil {
		t.Fatal(err)
	}
}

func TestUnlinkDevice(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	pl := newNamed(t, client, "a", "b", "c")
	dev, other := newDevice(t, client, 1), newDevice(t, client, 2)
	for _, id := range pl {
		for _, d := range []string{dev.FriendlyID, other.FriendlyID} {
			if err := LinkDevice(ctx, client, ByID(id), d); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := UnlinkDevice(ctx, client, ByID(pl[0]), dev.FriendlyID); err != nil {
		t.Fatal(err)
	}
	if got := links(t, client, dev.ID); !slices.Equal(got, pl[1:]) {
		t.Errorf("links = %v, want %v", got, pl[1:])
	}
	if got := links(t, client, other.ID); !slices.Equal(got, pl) {
		t.Errorf("another device's links = %v, want %v", got, pl)
	}
	for _, tc := range []struct {
		name     string
		pl       int64
		dev      string
		wantKind error
	}{
		{"already unlinked", pl[0], dev.FriendlyID, ErrNotLinked},
		{"unknown playlist", 999, dev.FriendlyID, ErrNoPlaylist},
		{"unknown device", pl[1], "ZZZ999", device.ErrNoDevice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := UnlinkDevice(ctx, client, ByID(tc.pl), tc.dev); !errors.Is(err, tc.wantKind) {
				t.Errorf("err = %v, want %v", err, tc.wantKind)
			}
		})
	}
}

// TestAddItemKeepsPriority checks that adding an item to a playlist that a
// device is linked to already leaves the link where it is, and that a new
// link goes last.
func TestAddItemKeepsPriority(t *testing.T) {
	t.Parallel()
	client, _, inst := newPlaylist(t, 1)
	ctx := t.Context()
	dev := newDevice(t, client, 1)
	p, _ := client.Read.GetPlaylistByName(ctx, "p")
	q, _ := client.Read.GetPlaylistByName(ctx, "q")
	for _, pl := range []db.Playlist{p, q} {
		if err := LinkDevice(ctx, client, ByID(pl.ID), dev.FriendlyID); err != nil {
			t.Fatal(err)
		}
	}
	// p is first. Adding to it must not move it, nor collide with q.
	if _, err := addItemTo(ctx, client, "p", dev.FriendlyID, inst); err != nil {
		t.Fatal(err)
	}
	if got := links(t, client, dev.ID); !slices.Equal(got, []int64{p.ID, q.ID}) {
		t.Errorf("links = %v, want p then q", got)
	}
	// An item for a playlist the device is not on links it last.
	if _, err := addItemTo(ctx, client, "", dev.FriendlyID, inst); err != nil {
		t.Fatal(err)
	}
	if got := links(t, client, dev.ID); len(got) != 3 || got[0] != p.ID || got[1] != q.ID {
		t.Errorf("links = %v", got)
	}
}

// TestDeletePlaylistManyDevices deletes a playlist that three devices have at
// different indexes, and checks that each device closes up around it and the
// device that is not linked to it is untouched.
func TestDeletePlaylistManyDevices(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	pl := newNamed(t, client, "a", "b", "c", "x")
	x := pl[3]
	devs := []db.Device{newDevice(t, client, 1), newDevice(t, client, 2), newDevice(t, client, 3), newDevice(t, client, 4)}
	// Orders: d1 has x at 1, d2 at 0, d3 at 3 (last), d4 is not on x.
	orders := [][]int64{{pl[0], x, pl[1], pl[2]}, {x, pl[2], pl[1]}, {pl[1], pl[0], pl[2], x}, {pl[2], pl[0]}}
	for i, order := range orders {
		for _, id := range order {
			if err := LinkDevice(ctx, client, ByID(id), devs[i].FriendlyID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := DeletePlaylist(ctx, client, ByID(x), nil); err != nil {
		t.Fatal(err)
	}
	for i, order := range orders {
		want := slices.DeleteFunc(slices.Clone(order), func(id int64) bool { return id == x })
		if got := links(t, client, devs[i].ID); !slices.Equal(got, want) {
			t.Errorf("device %d links = %v, want %v", i+1, got, want)
		}
	}
}

// TestMoveLinkLong moves links in a longer list, down, up and in place, to
// check that the ranking keeps everything else in order.
func TestMoveLinkLong(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	pl := newNamed(t, client, "a", "b", "c", "d", "e", "f")
	dev := newDevice(t, client, 1)
	for _, id := range pl {
		if err := LinkDevice(ctx, client, ByID(id), dev.FriendlyID); err != nil {
			t.Fatal(err)
		}
	}
	want := slices.Clone(pl)
	for _, m := range []struct{ from, to int }{{1, 4}, {5, 0}, {2, 2}, {0, 5}, {4, 3}} {
		id := want[m.from]
		want = slices.Insert(slices.Delete(want, m.from, m.from+1), m.to, id)
		to := int64(m.to)
		if err := MoveLink(ctx, client, ByID(id), dev.FriendlyID, links(t, client, dev.ID), to); err != nil {
			t.Fatal(err)
		}
		if got := links(t, client, dev.ID); !slices.Equal(got, want) {
			t.Fatalf("after moving %d to %d: %v, want %v", m.from, m.to, got, want)
		}
	}
}
