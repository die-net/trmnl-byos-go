package main

import (
	"bytes"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
)

// runPlaylist runs `playlist args...` against the data directory and returns
// its output.
func runPlaylist(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := playlists(t.Context(), append([]string{args[0], "-data", dir}, args[1:]...), &out)
	return out.String(), err
}

// playlistFixture returns a data directory with a playlist "p" of three items
// and an instance.
func playlistFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	client, err := openDB(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	rec, err := client.Write.CreateRecipe(t.Context(), db.CreateRecipeParams{Name: "r", Source: "s", RefreshInterval: 900, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := client.Write.CreateInstance(t.Context(), db.CreateInstanceParams{RecipeID: rec.ID, Name: "Home", FieldValues: "{}", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, _, err := playlist.AddItem(t.Context(), client, "p", "", inst.ID, playlist.DefaultDuration); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPlaylistMove(t *testing.T) {
	t.Parallel()
	dir := playlistFixture(t)
	if _, err := runPlaylist(t, dir, "move", "1", "2"); err != nil {
		t.Fatal(err)
	}
	out, err := runPlaylist(t, dir, "list")
	if err != nil {
		t.Fatal(err)
	}
	// Item 1 is last now, so the rows are 2, 3, 1 with indexes 0, 1, 2.
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 3 && f[0] == "p" {
			rows = append(rows, f[1]+"@"+f[2])
		}
	}
	if got := strings.Join(rows, " "); got != "2@0 3@1 1@2" {
		t.Errorf("items after the move: %q\n%s", got, out)
	}
}

// testTZ is the TZ the schedule tests run in. It is not UTC, so a date read
// in any other zone than TZ is caught.
const testTZ = "Asia/Kolkata"

// testLocation returns testTZ's zone.
func testLocation(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(testTZ)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestPlaylistSet(t *testing.T) {
	t.Setenv("TZ", testTZ)
	loc := testLocation(t)
	dir := playlistFixture(t)
	if _, err := runPlaylist(t, dir, "set", "2", "-duration", "30m", "-hidden", "-days", "mon,fri", "-window", "22:00-06:00", "-from", "2026-11-14", "-until", "2026-11-15"); err != nil {
		t.Fatal(err)
	}
	client, err := openDB(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	item, err := client.Read.GetPlaylistItem(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 11, 14, 0, 0, 0, 0, loc).Unix()
	until := time.Date(2026, 11, 16, 0, 0, 0, 0, loc).Unix()
	if item.Duration != 1800 || item.Hidden != 1 || item.Days != 0b0100010 || item.WindowStart != 1320 || item.WindowEnd != 360 || item.StartsAt != from || item.EndsAt != until {
		t.Fatalf("item = %+v; want from %d until %d", item, from, until)
	}
	// Only the flags given change, the ITEM may come last, and an empty
	// date clears its bound.
	if _, err := runPlaylist(t, dir, "set", "-hidden=false", "-until", "", "2"); err != nil {
		t.Fatal(err)
	}
	item, err = client.Read.GetPlaylistItem(t.Context(), 2)
	if err != nil || item.Hidden != 0 || item.EndsAt != 0 || item.StartsAt != from || item.Duration != 1800 || item.Days != 0b0100010 {
		t.Errorf("after a partial set: %+v, %v", item, err)
	}
	// A window of "all" clears the hours, and so does 0 for the duration.
	if _, err := runPlaylist(t, dir, "set", "2", "-window", "all", "-duration", "0"); err != nil {
		t.Fatal(err)
	}
	item, err = client.Read.GetPlaylistItem(t.Context(), 2)
	if err != nil || item.WindowStart != 0 || item.WindowEnd != 0 || item.Duration != 0 {
		t.Errorf("after window all: %+v, %v", item, err)
	}

	out, err := runPlaylist(t, dir, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mon,fri from 2026-11-14 00:00") {
		t.Errorf("list does not show the schedule:\n%s", out)
	}
}

func TestPlaylistSetRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		kind error
	}{
		{"unknown day", []string{"set", "2", "-days", "monday"}, errkind.ErrInvalid},
		{"empty days", []string{"set", "2", "-days", ""}, errkind.ErrInvalid},
		{"bad window", []string{"set", "2", "-window", "9-17"}, errkind.ErrInvalid},
		{"bad date", []string{"set", "2", "-from", "11/14/2026"}, errkind.ErrInvalid},
		{"until before from", []string{"set", "2", "-from", "2026-11-14", "-until", "2026-11-13"}, errkind.ErrInvalid},
		{"short duration", []string{"set", "2", "-duration", "5s"}, errkind.ErrInvalid},
		{"unknown item", []string{"set", "99", "-hidden"}, playlist.ErrNoItem},
		{"nothing to change", []string{"set", "2"}, nil},
		{"no item", []string{"set", "-hidden"}, nil},
		{"item is not a number", []string{"set", "x", "-hidden"}, nil},
		{"move out of range", []string{"move", "1", "3"}, errkind.ErrInvalid},
		{"move unknown item", []string{"move", "99", "0"}, playlist.ErrNoItem},
		{"move without an index", []string{"move", "1"}, nil},
		{"schedule unknown playlist", []string{"schedule", "nope", "-days", "mon"}, playlist.ErrNoPlaylist},
		{"schedule bad days", []string{"schedule", "p", "-days", "someday"}, errkind.ErrInvalid},
		{"schedule nothing to change", []string{"schedule", "p"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := playlistFixture(t)
			_, err := runPlaylist(t, dir, tc.args...)
			if err == nil || (tc.kind != nil && !errors.Is(err, tc.kind)) {
				t.Fatalf("err = %v, want kind %v", err, tc.kind)
			}
			// Nothing was changed.
			client, err := openDB(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			for _, id := range []int64{1, 2, 3} {
				item, err := client.Read.GetPlaylistItem(t.Context(), id)
				if err != nil || item.Position != id-1 || item.Hidden != 0 || item.Days != 127 || item.Duration != 900 || item.StartsAt != 0 || item.EndsAt != 0 {
					t.Errorf("item %d = %+v, %v", id, item, err)
				}
			}
			pl, err := client.Read.GetPlaylistByName(t.Context(), "p")
			if err != nil || pl.Days != 127 {
				t.Errorf("playlist = %+v, %v", pl, err)
			}
		})
	}
}

func TestPlaylistSchedule(t *testing.T) {
	t.Setenv("TZ", testTZ)
	loc := testLocation(t)
	dir := playlistFixture(t)
	if _, err := runPlaylist(t, dir, "schedule", "p", "-days", "sat,sun", "-window", "08:00-20:00", "-until", "2026-12-31"); err != nil {
		t.Fatal(err)
	}
	client, err := openDB(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	pl, err := client.Read.GetPlaylistByName(t.Context(), "p")
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, loc).Unix()
	if err != nil || pl.Days != 0b1000001 || pl.WindowStart != 480 || pl.WindowEnd != 1200 || pl.StartsAt != 0 || pl.EndsAt != until {
		t.Fatalf("playlist = %+v, %v; want until %d", pl, err, until)
	}
	out, err := runPlaylist(t, dir, "list")
	if err != nil || !strings.Contains(out, "sun,sat 08:00-20:00 until 2027-01-01 00:00") {
		t.Errorf("list: %v\n%s", err, out)
	}
	// Flags given change only their part.
	if _, err := runPlaylist(t, dir, "schedule", "p", "-window", "all"); err != nil {
		t.Fatal(err)
	}
	pl, err = client.Read.GetPlaylistByName(t.Context(), "p")
	if err != nil || pl.Days != 0b1000001 || pl.WindowStart != 0 || pl.WindowEnd != 0 || pl.EndsAt != until {
		t.Errorf("after window all: %+v, %v", pl, err)
	}
}

// TestServerZone checks that the zone is TZ, that an unset TZ is UTC, and
// that an unknown name is refused rather than read as UTC.
func TestServerZone(t *testing.T) {
	for _, tc := range []struct {
		tz, want string
		bad      bool
	}{
		{tz: testTZ, want: testTZ},
		{tz: "", want: "UTC"},
		{tz: "Mars/Olympus_Mons", bad: true},
	} {
		t.Run(tc.tz, func(t *testing.T) {
			t.Setenv("TZ", tc.tz)
			loc, err := serverZone()
			if tc.bad {
				if err == nil {
					t.Fatalf("TZ %q gave %v, want an error", tc.tz, loc)
				}
				return
			}
			if err != nil || loc.String() != tc.want {
				t.Fatalf("TZ %q gave %v, %v; want %s", tc.tz, loc, err, tc.want)
			}
		})
	}
}

// TestPlaylistManage runs create, rename, link, unlink and delete, and checks
// what each leaves in the database and what a refused one reports.
func TestPlaylistManage(t *testing.T) {
	t.Parallel()
	dir := playlistFixture(t)
	client, err := openDB(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, n := range []int{1, 2} {
		_, err := client.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
			Mac: "D8:3B:DA:12:34:0" + strconv.Itoa(n), ApiKeyHash: []byte("key " + strconv.Itoa(n)), FriendlyID: "ABC00" + strconv.Itoa(n), CreatedAt: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := runPlaylist(t, dir, args...)
		if err != nil {
			t.Fatalf("playlist %v: %v", args, err)
		}
		return out
	}
	run("create", "Mornings")
	run("rename", "Mornings", "Evenings")
	run("link", "Evenings", "ABC001")
	run("link", "p", "d8:3b:da:12:34:01")
	run("order", "p", "ABC001", "0")
	run("link", "Evenings", "ABC002")
	devices := func(friendly string) []string {
		t.Helper()
		devs, err := client.Read.ListPlaylistDevices(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, d := range devs {
			if d.FriendlyID == friendly {
				got = append(got, d.Playlist+"@"+strconv.FormatInt(d.Priority, 10))
			}
		}
		slices.Sort(got)
		return got
	}
	if got := devices("ABC001"); !slices.Equal(got, []string{"Evenings@1", "p@0"}) {
		t.Errorf("ABC001 links = %v", got)
	}
	// A negative index reaches the command and is refused as out of range, not
	// read as a flag, and nothing moves.
	for _, index := range []string{"-1", "2"} {
		if _, err := runPlaylist(t, dir, "order", "p", "ABC001", index); !errors.Is(err, errkind.ErrInvalid) || !strings.Contains(err.Error(), index+" is not a place in the device's playlist order") {
			t.Errorf("order %s: %v", index, err)
		}
	}
	if got := devices("ABC001"); !slices.Equal(got, []string{"Evenings@1", "p@0"}) {
		t.Errorf("ABC001 links after refused moves = %v", got)
	}
	run("unlink", "p", "ABC001")
	if got := devices("ABC001"); !slices.Equal(got, []string{"Evenings@0"}) {
		t.Errorf("ABC001 links after unlink = %v", got)
	}
	run("delete", "Evenings")
	if got := devices("ABC002"); len(got) != 0 {
		t.Errorf("ABC002 links after delete = %v", got)
	}
	if out := run("list"); strings.Contains(out, "Evenings") || strings.Contains(out, "Mornings") {
		t.Errorf("list shows a deleted playlist:\n%s", out)
	}

	for _, tc := range []struct {
		name string
		args []string
		kind error
	}{
		{"create duplicate", []string{"create", "p"}, playlist.ErrNameTaken},
		{"create empty", []string{"create", " "}, errkind.ErrInvalid},
		{"rename missing", []string{"rename", "nope", "x"}, playlist.ErrNoPlaylist},
		{"rename to taken", []string{"create", "q"}, nil},
		{"rename onto another", []string{"rename", "q", "p"}, playlist.ErrNameTaken},
		{"delete missing", []string{"delete", "nope"}, playlist.ErrNoPlaylist},
		{"link missing playlist", []string{"link", "nope", "ABC001"}, playlist.ErrNoPlaylist},
		{"link missing device", []string{"link", "p", "ZZZ999"}, device.ErrNoDevice},
		{"order not linked", []string{"order", "p", "ABC002", "0"}, playlist.ErrNotLinked},
		{"order missing device", []string{"order", "p", "ZZZ999", "0"}, device.ErrNoDevice},
		{"order missing playlist", []string{"order", "nope", "ABC001", "0"}, playlist.ErrNoPlaylist},
		{"move negative", []string{"move", "1", "-1"}, errkind.ErrInvalid},
		{"unlink missing device", []string{"unlink", "p", "ZZZ999"}, device.ErrNoDevice},
		{"unlink missing playlist", []string{"unlink", "nope", "ABC001"}, playlist.ErrNoPlaylist},
		{"schedule missing playlist", []string{"schedule", "nope", "-days", "mon"}, playlist.ErrNoPlaylist},
		{"unlink not linked", []string{"unlink", "p", "ABC002"}, playlist.ErrNotLinked},
	} {
		_, err := runPlaylist(t, dir, tc.args...)
		if tc.kind == nil && err != nil || tc.kind != nil && !errors.Is(err, tc.kind) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.kind)
		}
	}
	if devs, err := client.Read.ListPlaylistDevices(t.Context()); err != nil || len(devs) != 0 {
		t.Errorf("a refused link left %v, %v", devs, err)
	}
}
