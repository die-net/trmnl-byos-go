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
		if err := LinkDevice(ctx, client, ByID(l.pl), l.dev, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeletePlaylist(ctx, client, ByID(pq[0])); err != nil {
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
	if err := LinkDevice(ctx, client, ByID(p.ID), d2.FriendlyID, new(int64)); err != nil {
		t.Fatal(err)
	}
	if err := DeletePlaylist(ctx, client, ByID(p.ID)); err != nil {
		t.Fatal(err)
	}
	if got := links(t, client, d2.ID); !slices.Equal(got, []int64{pq[2], pq[1]}) {
		t.Errorf("d2 links after deleting p = %v", got)
	}
	if _, err := client.Read.GetPlaylistItem(ctx, ids[0]); err == nil {
		t.Error("an item of the deleted playlist is still there")
	}
	if err := DeletePlaylist(ctx, client, ByID(p.ID)); !errors.Is(err, ErrNoPlaylist) {
		t.Errorf("delete again: %v", err)
	}
}

func TestLinkDevice(t *testing.T) {
	t.Parallel()
	ptr := func(n int64) *int64 { return &n }
	// Each case starts with a device linked to a, b, c in that order, and a
	// playlist d that is not linked.
	for _, tc := range []struct {
		name     string
		playlist string
		priority *int64
		want     string // playlist names in order
		kind     error
	}{
		{"new, last", "d", nil, "abcd", nil},
		{"new, first", "d", ptr(0), "dabc", nil},
		{"new, middle", "d", ptr(2), "abdc", nil},
		{"new, explicit last", "d", ptr(3), "abcd", nil},
		{"new, past the end", "d", ptr(4), "abc", errkind.ErrInvalid},
		{"new, negative", "d", ptr(-1), "abc", errkind.ErrInvalid},
		{"existing, no priority", "b", nil, "abc", nil},
		{"existing, down", "a", ptr(2), "bca", nil},
		{"existing, up", "c", ptr(0), "cab", nil},
		{"existing, in place", "b", ptr(1), "abc", nil},
		{"existing, past the end", "a", ptr(3), "abc", errkind.ErrInvalid},
		{"existing, negative", "a", ptr(-1), "abc", errkind.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _, _ := newPlaylist(t, 1)
			ctx := t.Context()
			byName := map[string]int64{}
			for _, n := range []string{"a", "b", "c", "d"} {
				byName[n] = newNamed(t, client, n)[0]
			}
			dev := newDevice(t, client, 1)
			for _, n := range []string{"a", "b", "c"} {
				if err := LinkDevice(ctx, client, ByID(byName[n]), dev.FriendlyID, nil); err != nil {
					t.Fatal(err)
				}
			}
			err := LinkDevice(ctx, client, ByID(byName[tc.playlist]), dev.FriendlyID, tc.priority)
			if tc.kind != nil && !errors.Is(err, tc.kind) || tc.kind == nil && err != nil {
				t.Fatalf("err = %v, want %v", err, tc.kind)
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

func TestLinkDeviceRefused(t *testing.T) {
	t.Parallel()
	client, _, _ := newPlaylist(t, 1)
	ctx := t.Context()
	p, _ := client.Read.GetPlaylistByName(ctx, "p")
	dev := newDevice(t, client, 1)
	if err := LinkDevice(ctx, client, ByID(p.ID+100), dev.FriendlyID, nil); !errors.Is(err, ErrNoPlaylist) {
		t.Errorf("unknown playlist: %v", err)
	}
	if err := LinkDevice(ctx, client, ByID(p.ID), "ZZZ999", nil); !errors.Is(err, device.ErrNoDevice) {
		t.Errorf("unknown device: %v", err)
	}
	if err := LinkDevice(ctx, client, ByName("nope"), dev.FriendlyID, nil); !errors.Is(err, ErrNoPlaylist) {
		t.Errorf("unknown playlist name: %v", err)
	}
	// A device by MAC and a playlist by name work as well.
	if err := LinkDevice(ctx, client, ByName("p"), dev.Mac, nil); err != nil {
		t.Errorf("by name and MAC: %v", err)
	}
	if got := links(t, client, dev.ID); len(got) != 1 {
		t.Errorf("links = %v", got)
	}
	if err := UnlinkDevice(ctx, client, ByName("p"), dev.Mac); err != nil {
		t.Fatal(err)
	}
	// A priority that is refused leaves no link behind.
	five := int64(5)
	if err := LinkDevice(ctx, client, ByID(p.ID), dev.FriendlyID, &five); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("priority 5: %v", err)
	}
	if got := links(t, client, dev.ID); len(got) != 0 {
		t.Errorf("a refused link was made: %v", got)
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
			if err := LinkDevice(ctx, client, ByID(id), d, nil); err != nil {
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
		if err := LinkDevice(ctx, client, ByID(pl.ID), dev.FriendlyID, nil); err != nil {
			t.Fatal(err)
		}
	}
	// p is first. Adding to it must not move it, nor collide with q.
	if _, _, err := AddItem(ctx, client, "p", dev.FriendlyID, inst, DefaultDuration); err != nil {
		t.Fatal(err)
	}
	if got := links(t, client, dev.ID); !slices.Equal(got, []int64{p.ID, q.ID}) {
		t.Errorf("links = %v, want p then q", got)
	}
	// An item for a playlist the device is not on links it last.
	if _, name, err := AddItem(ctx, client, "", dev.FriendlyID, inst, DefaultDuration); err != nil || name != dev.FriendlyID {
		t.Fatalf("add to the device's playlist: %q, %v", name, err)
	}
	if got := links(t, client, dev.ID); len(got) != 3 || got[0] != p.ID || got[1] != q.ID {
		t.Errorf("links = %v", got)
	}
}

func TestAddItemName(t *testing.T) {
	t.Parallel()
	client, _, inst := newPlaylist(t, 1)
	ctx := t.Context()
	if _, name, err := AddItem(ctx, client, "  spaced ", "", inst, DefaultDuration); err != nil || name != "spaced" {
		t.Errorf("name = %q, %v", name, err)
	}
	if _, _, err := AddItem(ctx, client, "a\nb", "", inst, DefaultDuration); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("control character: %v", err)
	}
	if _, _, err := AddItem(ctx, client, "   ", "", inst, DefaultDuration); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("blank name: %v", err)
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
			if err := LinkDevice(ctx, client, ByID(id), devs[i].FriendlyID, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := DeletePlaylist(ctx, client, ByID(x)); err != nil {
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
		if err := LinkDevice(ctx, client, ByID(id), dev.FriendlyID, nil); err != nil {
			t.Fatal(err)
		}
	}
	want := slices.Clone(pl)
	for _, m := range []struct{ from, to int }{{1, 4}, {5, 0}, {2, 2}, {0, 5}, {4, 3}} {
		id := want[m.from]
		want = slices.Insert(slices.Delete(want, m.from, m.from+1), m.to, id)
		to := int64(m.to)
		if err := LinkDevice(ctx, client, ByID(id), dev.FriendlyID, &to); err != nil {
			t.Fatal(err)
		}
		if got := links(t, client, dev.ID); !slices.Equal(got, want) {
			t.Fatalf("after moving %d to %d: %v, want %v", m.from, m.to, got, want)
		}
	}
}
