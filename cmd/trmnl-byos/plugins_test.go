package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
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
		if _, _, err := playlist.AddItem(t.Context(), client, "p", 0, inst.ID, playlist.DefaultDuration); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPlaylistMove(t *testing.T) {
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

func TestPlaylistSet(t *testing.T) {
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
	from := time.Date(2026, 11, 14, 0, 0, 0, 0, time.Local).Unix()
	until := time.Date(2026, 11, 16, 0, 0, 0, 0, time.Local).Unix()
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
	dir := playlistFixture(t)
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
			_, err := runPlaylist(t, dir, tc.args...)
			if err == nil || (tc.kind != nil && !errors.Is(err, tc.kind)) {
				t.Fatalf("err = %v, want kind %v", err, tc.kind)
			}
		})
	}
	// Nothing was changed by any of them.
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
}

func TestPlaylistSchedule(t *testing.T) {
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
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local).Unix()
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
