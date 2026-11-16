package adminui

import (
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/adminauth"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
)

// addInstance adds an instance of a recipe and returns its ID.
func addInstance(t *testing.T, tu *testUI, name string) int64 {
	t.Helper()
	ctx := t.Context()
	recipes, err := tu.db.Read.ListRecipes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var recipeID int64
	if len(recipes) > 0 {
		recipeID = recipes[0].ID
	} else {
		r, err := tu.db.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "weather", Source: "x", RefreshInterval: 15, InstalledAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		recipeID = r.ID
	}
	inst, err := tu.db.Write.CreateInstance(ctx, db.CreateInstanceParams{RecipeID: recipeID, Name: name, FieldValues: "{}", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	return inst.ID
}

// addItem adds an item showing a new instance with the name to the named
// playlist, and returns the item.
func addItem(t *testing.T, tu *testUI, list, instance string, d time.Duration) db.PlaylistItem {
	t.Helper()
	item, err := addItemTo(t.Context(), tu.db, list, "", addInstance(t, tu, instance), d)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// listItems returns the items of the playlist "p" in order.
func listItems(t *testing.T, tu *testUI) []db.ListPlaylistItemsRow {
	t.Helper()
	all, err := tu.db.Read.ListPlaylistItems(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []db.ListPlaylistItemsRow
	for _, it := range all {
		if it.Playlist == "p" {
			out = append(out, it)
		}
	}
	return out
}

// playlistID returns the ID of the named playlist.
func playlistID(t *testing.T, tu *testUI, name string) int64 {
	t.Helper()
	pl, err := tu.db.Read.GetPlaylistByName(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return pl.ID
}

func itemNames(rows []db.ListPlaylistItemsRow) string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.InstanceName
	}
	return strings.Join(names, ",")
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

// seenFields are the hidden fields of the name and schedule form that say
// what the page showed.
var seenFields = []string{"seen_name", "seen_days", "seen_window_start", "seen_window_end", "seen_starts_at", "seen_ends_at"}

// hidden returns the value of the hidden field name on the page, unescaped,
// and fails the test if the page has none.
func hidden(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`<input type="hidden" name="` + name + `" value="([^"]*)">`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the page has no hidden field %s:\n%s", name, body)
	}
	return html.UnescapeString(m[1])
}

// withSeen adds to form the hidden fields of the playlist's page as it is
// now, as a browser that loaded the page would post them.
func withSeen(t *testing.T, tu *testUI, pid int64, form url.Values) url.Values {
	t.Helper()
	body := tu.get(t, tu.signIn(t), "/playlists/"+id(pid)).Body.String()
	for _, name := range seenFields {
		form.Set(name, hidden(t, body, name))
	}
	return form
}

// withContents adds to form the hidden fields of the playlist's delete
// confirm page as it is now.
func withContents(t *testing.T, tu *testUI, pid int64, form url.Values) url.Values {
	t.Helper()
	body := tu.get(t, tu.signIn(t), "/playlists/delete/"+id(pid)).Body.String()
	for _, name := range []string{"items", "devices"} {
		form.Set(name, hidden(t, body, name))
	}
	return form
}

func TestPlaylistsEmpty(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	body := tu.get(t, tu.signIn(t), "/playlists").Body.String()
	for _, want := range []string{
		"<h1>Playlists</h1>", "<h2>No playlists yet</h2>", "Make one with the form below.",
		`<a class="btn btn-secondary" href="#new">New playlist</a>`,
		`<section class="panel form-panel" id="new"`, `<form method="post" action="/playlists#new">`,
		`<input id="new_name" name="name" type="text" maxlength="64" autocomplete="off" required value="">`,
		`<button class="btn btn-primary" type="submit">Create</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty page lacks %q", want)
		}
	}
	if strings.Contains(body, `class="stack"`) {
		t.Error("empty page has a playlist list")
	}
}

func TestPlaylistsList(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	ctx := t.Context()
	addDevice(t, tu, testMAC, "ABC123", "Kitchen <b>", 0)
	addDevice(t, tu, "D8:3B:DA:00:00:02", "DEF456", "", 0)
	dev, err := device.FindDevice(ctx, tu.db.Read, "ABC123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := addItemTo(ctx, tu.db, "", dev.FriendlyID, addInstance(t, tu, "Solo"), 0); err != nil {
		t.Fatal(err)
	}
	other, err := device.FindDevice(ctx, tu.db.Read, "DEF456")
	if err != nil {
		t.Fatal(err)
	}
	// A second device on the same playlist, which has no name.
	pl, err := tu.db.Read.GetPlaylistByName(ctx, "ABC123")
	if err != nil {
		t.Fatal(err)
	}
	if err := playlist.LinkDevice(ctx, tu.db, playlist.ByID(pl.ID), other.FriendlyID); err != nil {
		t.Fatal(err)
	}

	const mornings = "Mornings & <i>x</i>"
	weather := addItem(t, tu, mornings, "Weather", 90*time.Minute)
	news := addItem(t, tu, mornings, "News <u>", 0)
	addItem(t, tu, mornings, "Quotes", 5*time.Minute)
	hidden := true
	weekdays, err := playlist.DaysFromNames([]string{"mon", "tue", "wed", "thu", "fri"})
	if err != nil {
		t.Fatal(err)
	}
	s := playlist.Schedule{Days: weekdays, WindowStart: 7 * 60, WindowEnd: 9 * 60}.Full()
	if err := playlist.UpdateItem(ctx, tu.db, weather.ID, playlist.ItemUpdate{Schedule: &s}); err != nil {
		t.Fatal(err)
	}
	if err := playlist.UpdateItem(ctx, tu.db, news.ID, playlist.ItemUpdate{Hidden: &hidden}); err != nil {
		t.Fatal(err)
	}
	// 2026-12-01 00:00 in the server's zone, which UTC would show as the day before.
	start := time.Date(2026, 12, 1, 0, 0, 0, 0, testZone).Unix()
	if err := playlist.UpdatePlaylist(ctx, tu.db, playlist.ByID(playlistID(t, tu, mornings)), playlist.Update{Schedule: &playlist.ScheduleEdit{StartsAt: &start}}); err != nil {
		t.Fatal(err)
	}

	w := tu.get(t, tu.signIn(t), "/playlists")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	inOrder := []string{
		`<h2>ABC123</h2>`, `<a href="/devices/ABC123">Kitchen &lt;b&gt;</a>, <a href="/devices/DEF456">DEF456</a>`, `Solo`, `Until a button press`,
		`<h2>Mornings &amp; &lt;i&gt;x&lt;/i&gt;</h2>`, `from 1 Dec 2026`,
		`Weather`, `1 h 30 min`, `Weekdays, 07:00 to 09:00`,
		`News &lt;u&gt;`, `<span class="badge badge-neutral">Hidden</span>`,
		`Quotes`, `5 min`,
	}
	pos := 0
	for _, want := range inOrder {
		i := strings.Index(body[pos:], want)
		if i < 0 {
			t.Fatalf("page lacks %q after offset %d:\n%s", want, pos, body)
		}
		pos += i + len(want)
	}
	for _, bad := range []string{"<i>x</i>", "<b>", "News <u>"} {
		if strings.Contains(body, bad) {
			t.Errorf("page has unescaped %q", bad)
		}
	}
	if n := strings.Count(body, "None. No device shows this playlist."); n != 1 {
		t.Errorf("%d playlists say that no device shows them, want 1", n)
	}
	if !strings.Contains(body, `id="new"`) || strings.Index(body, `id="new"`) < strings.LastIndex(body, `id="playlist-`) {
		t.Error("the new playlist panel is not after the list")
	}
	if n := strings.Count(body, "Hidden</span>"); n != 1 {
		t.Errorf("%d Hidden badges, want 1", n)
	}
	// The default schedule is not repeated on each row, and the first row
	// cannot move up nor the last move down.
	if strings.Count(body, `<span class="rel">always</span>`) != 0 {
		t.Error("an item with the default schedule shows it")
	}
	for _, want := range []string{
		`<form method="post" action="/playlists/items/` + id(weather.ID) + `/move"><input type="hidden" name="index" value="-1"><button class="btn btn-secondary btn-icon" type="submit" aria-label="Move Weather up" disabled>`,
		`<form method="post" action="/playlists/items/` + id(news.ID) + `/hidden"><input type="hidden" name="hidden" value="0">`,
		`<a class="btn btn-secondary" href="/playlists/items/` + id(news.ID) + `"`,
		`<a class="btn btn-secondary btn-danger" href="/playlists/items/` + id(news.ID) + `/delete" aria-label="Remove News &lt;u&gt;">Remove</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !strings.Contains(body, `aria-label="Move Quotes down" disabled`) {
		t.Error("the last item can move down")
	}
}

func TestPlaylistItemActions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		action string // the path after the item's.
		item   int    // which of a, b, c.
		form   url.Values
		order  string
		hidden string // the names of the hidden items.
	}{
		{"move up", "/move", 1, url.Values{"index": {"0"}}, "b,a,c", ""},
		{"move down", "/move", 1, url.Values{"index": {"2"}}, "a,c,b", ""},
		{"hide", "/hidden", 0, url.Values{"hidden": {"1"}}, "a,b,c", "a"},
		{"remove", "/delete", 1, url.Values{}, "a,c", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			items := []db.PlaylistItem{addItem(t, tu, "p", "a", 0), addItem(t, tu, "p", "b", 0), addItem(t, tu, "p", "c", 0)}
			w := tu.post(t, tu.signIn(t), "/playlists/items/"+id(items[tc.item].ID)+tc.action, tc.form)
			want := "/playlists#playlist-" + id(playlistID(t, tu, "p"))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
				t.Fatalf("status %d to %q, want 303 to %q: %s", w.Code, w.Header().Get("Location"), want, w.Body)
			}
			rows := listItems(t, tu)
			if got := itemNames(rows); got != tc.order {
				t.Errorf("order %s, want %s", got, tc.order)
			}
			var hidden []string
			for _, r := range rows {
				if r.Hidden != 0 {
					hidden = append(hidden, r.InstanceName)
				}
			}
			if got := strings.Join(hidden, ","); got != tc.hidden {
				t.Errorf("hidden %q, want %q", got, tc.hidden)
			}
		})
	}

	t.Run("show", func(t *testing.T) {
		t.Parallel()
		tu := newTestUI(t, Config{}, true)
		item := addItem(t, tu, "p", "a", 0)
		hidden := true
		if err := playlist.UpdateItem(t.Context(), tu.db, item.ID, playlist.ItemUpdate{Hidden: &hidden}); err != nil {
			t.Fatal(err)
		}
		c := tu.signIn(t)
		if body := tu.get(t, c, "/playlists").Body.String(); !strings.Contains(body, `name="hidden" value="0"`) {
			t.Fatal("the list has no Show form for a hidden item")
		}
		w := tu.post(t, c, "/playlists/items/"+id(item.ID)+"/hidden", url.Values{"hidden": {"0"}})
		if w.Code != http.StatusSeeOther {
			t.Fatalf("status %d", w.Code)
		}
		if rows := listItems(t, tu); rows[0].Hidden != 0 {
			t.Error("the item is still hidden")
		}
	})
}

func TestPlaylistItemActionRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		action string
		form   url.Values
		want   string
	}{
		{"index past the end", "/move", url.Values{"index": {"9"}}, "9 is not a place in the playlist, which runs from 0 to 1."},
		{"index not a number", "/move", url.Values{"index": {"x"}}, "is not a position"},
		{"no index", "/move", url.Values{}, "is not a position"},
		{"hidden not 0 or 1", "/hidden", url.Values{"hidden": {"yes"}}, "Choose Hide or Show."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			a, _ := addItem(t, tu, "p", "a", 0), addItem(t, tu, "p", "b", 0)
			w := tu.post(t, tu.signIn(t), "/playlists/items/"+id(a.ID)+tc.action, tc.form)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) || !strings.Contains(w.Body.String(), `role="alert"`) {
				t.Fatalf("status %d, body %s; want 400 with %q", w.Code, w.Body, tc.want)
			}
			if got := itemNames(listItems(t, tu)); got != "a,b" {
				t.Errorf("order %s after a refused change", got)
			}
		})
	}
}

func TestPlaylistAddItem(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addItem(t, tu, "p", "a", 0)
	inst := addInstance(t, tu, "Fresh <b>")
	pid := playlistID(t, tu, "p")
	c := tu.signIn(t)

	page := tu.get(t, c, "/playlists/"+id(pid)).Body.String()
	for _, want := range []string{`<form method="post" action="/playlists/add-item/` + id(pid) + `">`, `<option value="` + id(inst) + `">Fresh &lt;b&gt; (weather)</option>`, `name="duration"`, `value="900"`} {
		if !strings.Contains(page, want) {
			t.Errorf("add form lacks %q", want)
		}
	}
	w := tu.post(t, c, "/playlists/add-item/"+id(pid), url.Values{"instance": {id(inst)}, "duration": {"600"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/playlists#playlist-"+id(pid) {
		t.Fatalf("status %d to %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	rows := listItems(t, tu)
	if len(rows) != 2 || rows[1].InstanceID != inst || rows[1].Duration != 600 {
		t.Errorf("items %+v, want the new instance last at 600 seconds", rows)
	}
}

func TestPlaylistAddItemRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		form func(inst int64) url.Values
		want string
	}{
		{"too short", func(i int64) url.Values { return url.Values{"instance": {id(i)}, "duration": {"30"}} }, "is under 1m0s"},
		{"not a number", func(i int64) url.Values { return url.Values{"instance": {id(i)}, "duration": {"soon"}} }, "whole number of seconds"},
		{"negative", func(i int64) url.Values { return url.Values{"instance": {id(i)}, "duration": {"-5"}} }, "-5 seconds is not between"},
		{"too long", func(i int64) url.Values { return url.Values{"instance": {id(i)}, "duration": {"99999999999"}} }, "seconds is not between"},
		{"no instance", func(int64) url.Values { return url.Values{"duration": {"600"}} }, "Choose an instance."},
		{"instance gone", func(i int64) url.Values { return url.Values{"instance": {id(i + 50)}, "duration": {"600"}} }, "That instance no longer exists."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addItem(t, tu, "p", "a", 0)
			inst := addInstance(t, tu, "Fresh")
			form := tc.form(inst)
			w := tu.post(t, tu.signIn(t), "/playlists/add-item/"+id(playlistID(t, tu, "p")), form)
			body := w.Body.String()
			if w.Code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
				t.Fatalf("status %d, body %s; want 400 with %q", w.Code, body, tc.want)
			}
			if d := form.Get("duration"); d != "" && !strings.Contains(body, `value="`+d+`"`) {
				t.Errorf("the form lost the duration %q", d)
			}
			if got := len(listItems(t, tu)); got != 1 {
				t.Errorf("%d items after a refused add, want 1", got)
			}
		})
	}
}

// TestPlaylistAddItemNoPlaylist checks that an add for a playlist that is
// gone, such as one deleted after its page was shown, gets the not-found page
// and does not make the playlist again.
func TestPlaylistAddItemNoPlaylist(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addItem(t, tu, "p", "a", 0)
	inst := addInstance(t, tu, "Fresh")
	pid := playlistID(t, tu, "p")
	c := tu.signIn(t)
	if w := tu.post(t, c, "/playlists/delete/"+id(pid), withContents(t, tu, pid, url.Values{})); w.Code != http.StatusSeeOther {
		t.Fatalf("delete: status %d: %s", w.Code, w.Body)
	}
	w := tu.post(t, c, "/playlists/add-item/"+id(pid), url.Values{"instance": {id(inst)}, "duration": {"600"}})
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "No such playlist") {
		t.Fatalf("status %d, body %s; want 404 saying there is no such playlist", w.Code, w.Body)
	}
	lists, err := tu.db.Read.ListPlaylists(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 0 || len(listItems(t, tu)) != 0 {
		t.Errorf("%d playlists and %d items after an add to a deleted playlist, want none", len(lists), len(listItems(t, tu)))
	}
}

func TestPlaylistNoInstances(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	if _, err := tu.db.Write.CreatePlaylist(t.Context(), "empty"); err != nil {
		t.Fatal(err)
	}
	body := tu.get(t, tu.signIn(t), "/playlists/"+id(playlistID(t, tu, "empty"))).Body.String()
	if !strings.Contains(body, "There are no instances to add.") || strings.Contains(body, "<select") {
		t.Errorf("page for a server with no instances:\n%s", body)
	}
}

func TestPlaylistItemEdit(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	item := addItem(t, tu, "p", "Weather", 15*time.Minute)
	c := tu.signIn(t)
	path := "/playlists/items/" + id(item.ID)

	page := tu.get(t, c, path).Body.String()
	for _, want := range []string{"<h1>Weather</h1>", "Item in p", `name="duration"`, `value="900"`, `name="days" value="sun" checked`, `type="time"`, `type="date"`, "Duration in seconds"} {
		if !strings.Contains(page, want) {
			t.Errorf("edit page lacks %q", want)
		}
	}

	// The days are in the server's zone, not UTC: midnight there is 18:30 the day before in UTC.
	w := tu.post(t, c, path, url.Values{
		"duration": {"1800"}, "hidden": {"1"}, "days": {"mon", "wed"},
		"window_start": {"22:00"}, "window_end": {"06:30"}, "from": {"2026-11-20"}, "until": {"2026-11-22"},
	})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/playlists#playlist-"+id(item.PlaylistID) {
		t.Fatalf("status %d to %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	got, err := tu.db.Read.GetPlaylistItem(t.Context(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	days, err := playlist.DaysFromNames([]string{"mon", "wed"})
	if err != nil {
		t.Fatal(err)
	}
	wantFrom := time.Date(2026, 11, 20, 0, 0, 0, 0, testZone).Unix()
	wantUntil := time.Date(2026, 11, 23, 0, 0, 0, 0, testZone).Unix()
	if got.Duration != 1800 || got.Hidden != 1 || got.Days != days || got.WindowStart != 22*60 || got.WindowEnd != 6*60+30 || got.StartsAt != wantFrom || got.EndsAt != wantUntil {
		t.Errorf("item %+v, want 1800 s hidden on mon,wed 22:00-06:30 from %d to %d", got, wantFrom, wantUntil)
	}

	// The stored values come back into the form, with the last day as the
	// admin typed it.
	page = tu.get(t, c, path).Body.String()
	for _, want := range []string{`value="1800"`, `name="hidden" value="1" checked`, `name="days" value="mon" checked`, `name="days" value="tue">`, `value="22:00"`, `value="06:30"`, `value="2026-11-20"`, `value="2026-11-22"`} {
		if !strings.Contains(page, want) {
			t.Errorf("edit page after saving lacks %q", want)
		}
	}

	// Clearing the fields restores the defaults.
	w = tu.post(t, c, path, url.Values{
		"duration": {"0"}, "days": {"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, "window_start": {""}, "window_end": {""}, "from": {""}, "until": {""},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	got, _ = tu.db.Read.GetPlaylistItem(t.Context(), item.ID)
	if got.Duration != 0 || got.Hidden != 0 || got.Days != playlist.AllDays || got.WindowStart != 0 || got.WindowEnd != 0 || got.StartsAt != 0 || got.EndsAt != 0 {
		t.Errorf("item %+v, want the defaults", got)
	}
}

// invalidCases are posts that the schedule fields refuse, with the message
// to show.
var invalidCases = []struct {
	name string
	set  func(v url.Values)
	want string
}{
	{"no days", func(v url.Values) { v.Del("days") }, "Days must name at least one weekday."},
	{"window start only", func(v url.Values) { v.Set("window_end", "") }, "Give both a start and an end"},
	{"window not a time", func(v url.Values) { v.Set("window_start", "25:00") }, "is not a time as HH:MM"},
	{"date not a date", func(v url.Values) { v.Set("from", "2026-13-40") }, "is not a date as YYYY-MM-DD"},
	{"from after until", func(v url.Values) { v.Set("from", "2026-12-05"); v.Set("until", "2026-12-01") }, "The first day is after the last day."},
}

// validPost is a post that the item form accepts.
func validPost() url.Values {
	return url.Values{"duration": {"1200"}, "days": {"tue", "thu"}, "window_start": {"08:00"}, "window_end": {"18:00"}, "from": {"2026-12-01"}, "until": {"2026-12-31"}}
}

func TestPlaylistItemEditRefused(t *testing.T) {
	t.Parallel()
	cases := append([]struct {
		name string
		set  func(v url.Values)
		want string
	}{
		{"duration not a number", func(v url.Values) { v.Set("duration", "ten") }, "whole number of seconds"},
		{"duration too short", func(v url.Values) { v.Set("duration", "59") }, "is under 1m0s"},
		{"duration negative", func(v url.Values) { v.Set("duration", "-1") }, "-1 seconds is not between"},
	}, invalidCases...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			item := addItem(t, tu, "p", "Weather", 15*time.Minute)
			form := validPost()
			tc.set(form)
			w := tu.post(t, tu.signIn(t), "/playlists/items/"+id(item.ID), form)
			body := w.Body.String()
			if w.Code != http.StatusBadRequest || !strings.Contains(body, tc.want) || !strings.Contains(body, `role="alert"`) {
				t.Fatalf("status %d, body %s; want 400 with %q", w.Code, body, tc.want)
			}
			// What was typed stays in the form.
			for _, key := range []string{"duration", "window_start", "window_end", "from", "until"} {
				if v := form.Get(key); v != "" && !strings.Contains(body, `value="`+v+`"`) {
					t.Errorf("the form lost %s=%q", key, v)
				}
			}
			for _, d := range form["days"] {
				if !strings.Contains(body, `value="`+d+`" checked`) {
					t.Errorf("the form lost the checked day %s", d)
				}
			}
			if len(form["days"]) == 2 && strings.Contains(body, `value="mon" checked`) {
				t.Error("the form checks a day that was not posted")
			}
			got, _ := tu.db.Read.GetPlaylistItem(t.Context(), item.ID)
			if got.Duration != 900 || got.Days != playlist.AllDays || got.StartsAt != 0 {
				t.Errorf("item %+v changed by a refused edit", got)
			}
		})
	}
}

func TestPlaylistScheduleEdit(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addItem(t, tu, "p", "a", 0)
	pid := playlistID(t, tu, "p")
	c := tu.signIn(t)
	path := "/playlists/" + id(pid)

	form := withSeen(t, tu, pid, validPost())
	form.Set("name", "p")
	form.Set("until", "2026-12-31")
	w := tu.post(t, c, path, form)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/playlists#playlist-"+id(pid) {
		t.Fatalf("status %d to %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	pl, err := tu.db.Read.GetPlaylist(t.Context(), pid)
	if err != nil {
		t.Fatal(err)
	}
	days, _ := playlist.DaysFromNames([]string{"tue", "thu"})
	from := time.Date(2026, 12, 1, 0, 0, 0, 0, testZone).Unix()
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, testZone).Unix()
	if pl.Days != days || pl.WindowStart != 8*60 || pl.WindowEnd != 18*60 || pl.StartsAt != from || pl.EndsAt != until {
		t.Errorf("playlist %+v, want tue,thu 08:00-18:00 from %d to %d", pl, from, until)
	}
	body := tu.get(t, c, "/playlists").Body.String()
	if want := "Tue, Thu, 08:00 to 18:00, 1 Dec 2026 to 31 Dec 2026"; !strings.Contains(body, want) {
		t.Errorf("list lacks %q", want)
	}
	if page := tu.get(t, c, path).Body.String(); !strings.Contains(page, `value="2026-12-31"`) || !strings.Contains(page, `value="08:00"`) {
		t.Error("the page does not show the saved schedule")
	}
}

func TestPlaylistScheduleEditRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addItem(t, tu, "p", "a", 0)
			pid := playlistID(t, tu, "p")
			form := withSeen(t, tu, pid, validPost())
			form.Set("name", "p")
			tc.set(form)
			w := tu.post(t, tu.signIn(t), "/playlists/"+id(pid), form)
			body := w.Body.String()
			if w.Code != http.StatusBadRequest || !strings.Contains(body, tc.want) || !strings.Contains(body, `role="alert"`) {
				t.Fatalf("status %d, body %s; want 400 with %q", w.Code, body, tc.want)
			}
			for _, key := range []string{"window_start", "window_end", "from", "until"} {
				if v := form.Get(key); v != "" && !strings.Contains(body, `value="`+v+`"`) {
					t.Errorf("the form lost %s=%q", key, v)
				}
			}
			pl, _ := tu.db.Read.GetPlaylist(t.Context(), pid)
			if pl.Days != playlist.AllDays || pl.WindowStart != 0 || pl.StartsAt != 0 {
				t.Errorf("playlist %+v changed by a refused edit", pl)
			}
		})
	}
}

// TestPlaylistDatesUseServerZone checks that a day is read in the server's
// zone: a UTC reading would be 5 hours 30 minutes off.
func TestPlaylistDatesUseServerZone(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addItem(t, tu, "p", "a", 0)
	pid := playlistID(t, tu, "p")
	form := withSeen(t, tu, pid, validPost())
	form.Set("name", "p")
	form.Set("from", "2026-12-01")
	form.Set("until", "2026-12-01")
	if w := tu.post(t, tu.signIn(t), "/playlists/"+id(pid), form); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	pl, _ := tu.db.Read.GetPlaylist(t.Context(), pid)
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, testZone).Unix(); pl.StartsAt != want {
		t.Errorf("starts_at %d, want %d", pl.StartsAt, want)
	}
	// A single day is a whole day: from the first midnight to the next.
	if pl.EndsAt-pl.StartsAt != 24*3600 {
		t.Errorf("a one-day schedule is %d seconds long, want 86400", pl.EndsAt-pl.StartsAt)
	}
}

func TestPlaylistNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method, path, want string
	}{
		{"GET", "/playlists/99", "No such playlist"},
		{"GET", "/playlists/abc", "No such playlist"},
		{"POST", "/playlists/99", "No such playlist"},
		{"POST", "/playlists/abc", "No such playlist"},
		{"POST", "/playlists/add-item/99", "No such playlist"},
		{"POST", "/playlists/add-item/abc", "No such playlist"},
		{"GET", "/playlists/delete/99", "No such playlist"},
		{"GET", "/playlists/delete/abc", "No such playlist"},
		{"POST", "/playlists/delete/99", "No such playlist"},
		{"POST", "/playlists/delete/abc", "No such playlist"},
		{"POST", "/playlists/link/99", "No such playlist"},
		{"POST", "/playlists/link/abc", "No such playlist"},
		{"POST", "/playlists/unlink/99", "No such playlist"},
		{"POST", "/playlists/unlink/abc", "No such playlist"},
		{"GET", "/playlists/items/99", "No such playlist item"},
		{"GET", "/playlists/items/abc", "No such playlist item"},
		{"GET", "/playlists/items/99/delete", "No such playlist item"},
		{"GET", "/playlists/items/abc/delete", "No such playlist item"},
		{"POST", "/playlists/items/99", "No such playlist item"},
		{"POST", "/playlists/items/abc", "No such playlist item"},
		{"POST", "/playlists/items/99/move", "No such playlist item"},
		{"POST", "/playlists/items/abc/move", "No such playlist item"},
		{"POST", "/playlists/items/99/hidden", "No such playlist item"},
		{"POST", "/playlists/items/99/delete", "No such playlist item"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addItem(t, tu, "p", "a", 0)
			addDevice(t, tu, testMAC, "ABC123", "", 0)
			w := tu.get(t, tu.signIn(t), tc.path)
			if tc.method == "POST" {
				w = tu.post(t, tu.signIn(t), tc.path, url.Values{"duration": {"600"}, "days": {"mon"}, "index": {"0"}, "hidden": {"1"}, "device": {"ABC123"}, "name": {"x"}})
			}
			body := w.Body.String()
			if w.Code != http.StatusNotFound || !strings.Contains(body, tc.want) || !strings.Contains(body, `<nav class="nav"`) {
				t.Errorf("status %d, body %s; want a 404 page in the shell with %q", w.Code, body, tc.want)
			}
			if got := len(listItems(t, tu)); got != 1 {
				t.Errorf("%d items left, want 1", got)
			}
			if got := len(playlistNames(t, tu)); got != 1 {
				t.Errorf("%d playlists after a request for one that is not there, want 1", got)
			}
		})
	}
}

func TestPlaylistsNeedSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/playlists"},
		{"POST", "/playlists"},
		{"GET", "/playlists/1"},
		{"POST", "/playlists/1"},
		{"GET", "/playlists/delete/1"},
		{"POST", "/playlists/delete/1"},
		{"POST", "/playlists/link/1"},
		{"POST", "/playlists/unlink/1"},
		{"POST", "/playlists/add-item/1"},
		{"GET", "/playlists/items/1"},
		{"POST", "/playlists/items/1"},
		{"POST", "/playlists/items/1/move"},
		{"POST", "/playlists/items/1/hidden"},
		{"POST", "/playlists/items/1/delete"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			item := addItem(t, tu, "p", "a", 0)
			addDevice(t, tu, testMAC, "ABC123", "", 0)
			body := ""
			if tc.method == "POST" {
				body = "index=0&hidden=1&name=new&device=ABC123"
			}
			for _, cookie := range []string{"", adminauth.CookieName + "=nope"} {
				var h []string
				if cookie != "" {
					h = []string{"Cookie", cookie}
				}
				w := tu.do(t, tc.method, tc.path, body, h...)
				if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
					t.Errorf("cookie %q: status %d to %q, want 303 to /login", cookie, w.Code, w.Header().Get("Location"))
				}
			}
			if rows := listItems(t, tu); len(rows) != 1 || rows[0].Hidden != 0 || rows[0].ID != item.ID {
				t.Errorf("items %+v changed without a session", rows)
			}
			if names := playlistNames(t, tu); len(names) != 1 || names[0] != "p" {
				t.Errorf("playlists %v changed without a session", names)
			}
			if ids := linkedIDs(t, tu, "ABC123"); len(ids) != 0 {
				t.Errorf("device links %v changed without a session", ids)
			}
		})
	}
}

func TestPlaylistsCrossOrigin(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	a, b := addItem(t, tu, "p", "a", 0), addInstance(t, tu, "b")
	pid := playlistID(t, tu, "p")
	c := tu.signIn(t)
	addDevice(t, tu, testMAC, "ABC123", "", 0)
	form := url.Values{"index": {"0"}, "hidden": {"1"}, "duration": {"600"}, "days": {"mon"}, "instance": {id(b)}, "name": {"new"}, "device": {"ABC123"}}
	for _, path := range []string{
		"/playlists", "/playlists/" + id(pid), "/playlists/add-item/" + id(pid),
		"/playlists/delete/" + id(pid), "/playlists/link/" + id(pid), "/playlists/unlink/" + id(pid), "/playlists/items/" + id(a.ID),
		"/playlists/items/" + id(a.ID) + "/move", "/playlists/items/" + id(a.ID) + "/hidden", "/playlists/items/" + id(a.ID) + "/delete",
	} {
		if w := tu.post(t, c, path, form, "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden {
			t.Errorf("cross-origin post to %s: status %d, want 403", path, w.Code)
		}
	}
	rows := listItems(t, tu)
	pl, _ := tu.db.Read.GetPlaylist(t.Context(), pid)
	if len(rows) != 1 || rows[0].Hidden != 0 || rows[0].Duration != 0 || pl.Days != playlist.AllDays || pl.Name != "p" {
		t.Errorf("a cross-origin post changed the playlist: %+v, %+v", rows, pl)
	}
	if names := playlistNames(t, tu); len(names) != 1 {
		t.Errorf("playlists %v after cross-origin posts, want only p", names)
	}
	if ids := linkedIDs(t, tu, "ABC123"); len(ids) != 0 {
		t.Errorf("a cross-origin post linked the device: %v", ids)
	}
}

func TestPlaylistFormTooBig(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	item := addItem(t, tu, "p", "a", 0)
	big := url.Values{"duration": {strings.Repeat("1", maxFormBody)}}
	for _, path := range []string{"/playlists", "/playlists/link/" + id(playlistID(t, tu, "p")), "/playlists/unlink/" + id(playlistID(t, tu, "p")), "/playlists/items/" + id(item.ID), "/playlists/" + id(playlistID(t, tu, "p")), "/playlists/items/" + id(item.ID) + "/move"} {
		if w := tu.post(t, tu.signIn(t), path, big); w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("POST %s: status %d, want 413", path, w.Code)
		}
	}
}

func TestPlaylistPagesNoInlineCode(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	item := addItem(t, tu, "p", "a", 0)
	c := tu.signIn(t)
	for _, path := range []string{"/playlists", "/playlists/" + id(playlistID(t, tu, "p")), "/playlists/items/" + id(item.ID), "/playlists/items/" + id(item.ID) + "/delete", "/playlists/delete/" + id(playlistID(t, tu, "p")), "/playlists/99"} {
		body := tu.get(t, c, path).Body.String()
		for _, bad := range []string{"<script>", "style=", "<style", " onclick=", " onsubmit="} {
			if strings.Contains(body, bad) {
				t.Errorf("%s has inline %q, which the CSP refuses", path, bad)
			}
		}
		if n := strings.Count(body, "<h1>"); n != 1 {
			t.Errorf("%s has %d h1 elements, want 1", path, n)
		}
		if strings.Count(body, `aria-current="page"`) != 1 || !strings.Contains(body, `<a href="/playlists" aria-current="page">`) {
			t.Errorf("%s does not mark Playlists as the current page", path)
		}
	}
}

func TestDevicePageLinksToPlaylists(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	if body := tu.get(t, tu.signIn(t), "/devices/ABC123").Body.String(); !strings.Contains(body, `<a href="/playlists">All playlists</a>`) {
		t.Error("the device page does not link to /playlists")
	}
}

func TestPlaylistDeleteConfirm(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	item := addItem(t, tu, "Rooms <i>", "News & <b>", 0)
	c := tu.signIn(t)
	path := "/playlists/items/" + id(item.ID) + "/delete"
	w := tu.get(t, c, path)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	for _, want := range []string{
		"Remove News &amp; &lt;b&gt; from Rooms &lt;i&gt;?",
		`<form method="post" action="` + path + `">`,
		`<a class="btn btn-secondary" href="/playlists#playlist-` + id(item.PlaylistID) + `">Cancel</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm page lacks %q", want)
		}
	}
	if strings.Contains(body, "<b>") || strings.Contains(body, "<i>") {
		t.Error("confirm page has unescaped names")
	}
	all, err := tu.db.Read.ListPlaylistItems(t.Context())
	if err != nil || len(all) != 1 {
		t.Fatalf("GET removed the item: %d items, %v", len(all), err)
	}
	if w := tu.post(t, c, path, url.Values{}); w.Code != http.StatusSeeOther {
		t.Fatalf("POST status %d", w.Code)
	}
	if all, _ = tu.db.Read.ListPlaylistItems(t.Context()); len(all) != 0 {
		t.Error("POST did not remove the item")
	}
}

func TestDescribeSchedule(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	day := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 0, 0, 0, 0, testZone).Unix() }
	days := func(names ...string) int64 {
		n, err := playlist.DaysFromNames(names)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, tc := range []struct {
		name string
		sc   playlist.Schedule
		want string
	}{
		{"default", playlist.Always, "Every day, all day"},
		{"weekdays", playlist.Schedule{Days: days("mon", "tue", "wed", "thu", "fri"), WindowStart: 6 * 60, WindowEnd: 9 * 60, EndsAt: day(2026, 12, 25)}, "Weekdays, 06:00 to 09:00, until 24 Dec 2026"},
		{"weekends", playlist.Schedule{Days: days("sat", "sun")}, "Weekends, all day"},
		{"run", playlist.Schedule{Days: days("mon", "tue", "wed", "thu")}, "Mon to Thu, all day"},
		{"run of two is a list", playlist.Schedule{Days: days("mon", "tue")}, "Mon, Tue, all day"},
		{"gap is a list", playlist.Schedule{Days: days("mon", "wed", "fri")}, "Mon, Wed, Fri, all day"},
		{"does not wrap", playlist.Schedule{Days: days("sat", "sun", "mon")}, "Sun, Mon, Sat, all day"},
		{"wraps midnight", playlist.Schedule{Days: playlist.AllDays, WindowStart: 22 * 60, WindowEnd: 6 * 60}, "Every day, 22:00 to 06:00 the next day"},
		{"from", playlist.Schedule{Days: playlist.AllDays, StartsAt: day(2026, 12, 3)}, "Every day, all day, from 3 Dec 2026"},
		{"both dates", playlist.Schedule{Days: playlist.AllDays, StartsAt: day(2026, 12, 3), EndsAt: day(2026, 12, 25)}, "Every day, all day, 3 Dec 2026 to 24 Dec 2026"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tu.srv.describeSchedule(tc.sc); got != tc.want {
				t.Errorf("describeSchedule = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildPlaylistViews checks that rows are joined to their playlist by
// ID, and that a row for a playlist that is not in the list is skipped.
func TestBuildPlaylistViews(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, false)
	lists := []db.Playlist{{ID: 1, Name: "same", Days: int64(playlist.AllDays)}, {ID: 2, Name: "other", Days: int64(playlist.AllDays)}}
	devs := []db.ListPlaylistDevicesRow{
		{Playlist: "other", PlaylistID: 1, FriendlyID: "ABC123"}, // The name is stale. The ID decides.
		{Playlist: "ghost", PlaylistID: 9, FriendlyID: "ABC124"},
	}
	items := []db.ListPlaylistItemsRow{
		{Playlist: "same", PlaylistID: 2, ID: 7, Days: int64(playlist.AllDays)},
		{Playlist: "ghost", PlaylistID: 9, ID: 8},
	}
	got := tu.srv.buildPlaylistViews(lists, devs, items)
	if len(got) != 2 || len(got[0].Devices) != 1 || len(got[0].Items) != 0 || len(got[1].Devices) != 0 || len(got[1].Items) != 1 || got[1].Items[0].ID != 7 {
		t.Errorf("views %+v", got)
	}
}

// playlistNames returns the names of every playlist, in name order.
func playlistNames(t *testing.T, tu *testUI) []string {
	t.Helper()
	all, err := tu.db.Read.ListPlaylists(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(all))
	for i, pl := range all {
		names[i] = pl.Name
	}
	return names
}

// linkedIDs returns the IDs of the playlists the device with the friendly ID
// is linked to, in the order it tries them.
func linkedIDs(t *testing.T, tu *testUI, friendly string) []int64 {
	t.Helper()
	dev, err := device.FindDevice(t.Context(), tu.db.Read, friendly)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := tu.db.Read.ListDevicePlaylistIDs(t.Context(), dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// newPlaylist makes an empty playlist and returns its ID.
func newPlaylist(t *testing.T, tu *testUI, name string) int64 {
	t.Helper()
	pl, err := playlist.CreatePlaylist(t.Context(), tu.db, name)
	if err != nil {
		t.Fatal(err)
	}
	return pl.ID
}

// linkTo links the device to the playlists, in the order given, last each time.
func linkTo(t *testing.T, tu *testUI, friendly string, playlists ...int64) {
	t.Helper()
	for _, pid := range playlists {
		if err := playlist.LinkDevice(t.Context(), tu.db, playlist.ByID(pid), friendly); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlaylistCreate(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	w := tu.post(t, tu.signIn(t), "/playlists", url.Values{"name": {"  Kitchen <b>x</b> "}})
	pl, err := tu.db.Read.GetPlaylistByName(t.Context(), "Kitchen <b>x</b>")
	if err != nil {
		t.Fatalf("the playlist was not made with its name trimmed: %v", err)
	}
	if want := "/playlists/" + id(pl.ID); w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
		t.Errorf("status %d to %q, want 303 to %q", w.Code, w.Header().Get("Location"), want)
	}
	if pl.Days != playlist.AllDays || pl.WindowStart != 0 || pl.StartsAt != 0 {
		t.Errorf("playlist %+v, want the default schedule", pl)
	}
	// The page it goes to is the one that adds items and links devices.
	body := tu.get(t, tu.signIn(t), w.Header().Get("Location")).Body.String()
	if !strings.Contains(body, "<h1>Kitchen &lt;b&gt;x&lt;/b&gt;</h1>") || !strings.Contains(body, "No device shows this playlist.") {
		t.Errorf("the new playlist's page:\n%s", body)
	}
}

func TestPlaylistCreateRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		typed  string
		status int
		want   string
	}{
		{"empty", "", http.StatusBadRequest, "Name is empty."},
		{"only space", "   ", http.StatusBadRequest, "Name is empty."},
		{"too long", strings.Repeat("a", 65), http.StatusBadRequest, "Name is over 64 bytes."},
		{"control character", "a\nb", http.StatusBadRequest, "Name has a control character."},
		{"taken", "<b>x</b>", http.StatusConflict, "A playlist has that name."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			newPlaylist(t, tu, "<b>x</b>")
			w := tu.post(t, tu.signIn(t), "/playlists", url.Values{"name": {tc.typed}})
			body := w.Body.String()
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, body)
			}
			// The reason is in the panel, above the field, and the name stays.
			wantInOrder(t, body, `id="new"`, `<p class="error" role="alert">`+tc.want+`</p>`, `id="new_name"`)
			if strings.Index(body, `role="alert"`) < strings.Index(body, `id="new"`) {
				t.Error("a message is above the panel")
			}
			wantInput := `value="` + strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(tc.typed) + `"`
			if !strings.Contains(body, wantInput) {
				t.Errorf("the form lost the typed name, want %s in:\n%s", wantInput, body)
			}
			if names := playlistNames(t, tu); len(names) != 1 {
				t.Errorf("playlists %v after a refused create, want only the first", names)
			}
		})
	}
}

func TestPlaylistSaveNameAndSchedule(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addItem(t, tu, "p", "a", 0)
	pid := playlistID(t, tu, "p")
	c := tu.signIn(t)

	page := tu.get(t, c, "/playlists/"+id(pid)).Body.String()
	wantInOrder(t, page, `<h2 id="schedule-title">Name and schedule</h2>`, `<label for="name">Name</label>`,
		`<input id="name" name="name" type="text" maxlength="64" autocomplete="off" required value="p">`, `name="days"`)
	// The form says what the page showed: the name and the stored schedule.
	for name, want := range map[string]string{
		"seen_name": "p", "seen_days": id(playlist.AllDays), "seen_window_start": "0", "seen_window_end": "0", "seen_starts_at": "0", "seen_ends_at": "0",
	} {
		if got := hidden(t, page, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	form := withSeen(t, tu, pid, validPost())
	form.Set("name", "  Mornings & <i>x</i> ")
	w := tu.post(t, c, "/playlists/"+id(pid), form)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/playlists#playlist-"+id(pid) {
		t.Fatalf("status %d to %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	pl, err := tu.db.Read.GetPlaylist(t.Context(), pid)
	if err != nil {
		t.Fatal(err)
	}
	days, _ := playlist.DaysFromNames([]string{"tue", "thu"})
	if pl.Name != "Mornings & <i>x</i>" || pl.Days != days || pl.WindowStart != 8*60 {
		t.Errorf("playlist %+v, want the new name and schedule", pl)
	}
	// Saving with the name it has is not a clash.
	withSeen(t, tu, pid, form)
	form.Set("name", "Mornings & <i>x</i>")
	if w := tu.post(t, c, "/playlists/"+id(pid), form); w.Code != http.StatusSeeOther {
		t.Errorf("saving under the same name: status %d", w.Code)
	}
}

func TestPlaylistSaveRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		set    func(v url.Values)
		status int
		want   string
	}{
		{"name taken", func(v url.Values) { v.Set("name", "other") }, http.StatusConflict, "A playlist has that name."},
		{"name empty", func(v url.Values) { v.Set("name", " ") }, http.StatusBadRequest, "Name is empty."},
		{"name too long", func(v url.Values) { v.Set("name", strings.Repeat("a", 65)) }, http.StatusBadRequest, "Name is over 64 bytes."},
		{"bad schedule with a new name", func(v url.Values) { v.Set("name", "fresh"); v.Del("days") }, http.StatusBadRequest, "Days must name at least one weekday."},
		{"bad window with a new name", func(v url.Values) { v.Set("name", "fresh"); v.Set("window_end", "") }, http.StatusBadRequest, "Give both a start and an end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addItem(t, tu, "p", "a", 0)
			newPlaylist(t, tu, "other")
			pid := playlistID(t, tu, "p")
			form := withSeen(t, tu, pid, validPost())
			tc.set(form)
			w := tu.post(t, tu.signIn(t), "/playlists/"+id(pid), form)
			body := w.Body.String()
			if w.Code != tc.status || !strings.Contains(body, tc.want) || !strings.Contains(body, `role="alert"`) {
				t.Fatalf("status %d, body %s; want %d with %q", w.Code, body, tc.status, tc.want)
			}
			// The heading is the stored name, and the form has what was typed.
			if !strings.Contains(body, "<h1>p</h1>") {
				t.Error("the heading is not the stored name")
			}
			if v := strings.TrimSpace(form.Get("name")); v != "" && !strings.Contains(body, `required value="`+v+`"`) {
				t.Errorf("the form lost the name %q", v)
			}
			for _, key := range []string{"window_start", "window_end", "from", "until"} {
				if v := form.Get(key); v != "" && !strings.Contains(body, `value="`+v+`"`) {
					t.Errorf("the form lost %s=%q", key, v)
				}
			}
			// Nothing changed, not the schedule that came with the name.
			pl, _ := tu.db.Read.GetPlaylist(t.Context(), pid)
			if pl.Name != "p" || pl.Days != playlist.AllDays || pl.WindowStart != 0 || pl.WindowEnd != 0 || pl.StartsAt != 0 || pl.EndsAt != 0 {
				t.Errorf("playlist %+v changed by a refused save", pl)
			}
		})
	}
}

// TestPlaylistSaveStale saves the name and schedule form from a page that
// another change has made out of date, in both directions the reviewer found.
// Each is refused with 409, nothing is written, and the page it shows keeps
// the typed values, says what the playlist is now, and holds the current
// values in its hidden fields, so that saving again applies the typed ones.
func TestPlaylistSaveStale(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addItem(t, tu, "Morning", "a", 0)
	pid := playlistID(t, tu, "Morning")
	c := tu.signIn(t)
	path := "/playlists/" + id(pid)
	stored := func() db.Playlist {
		t.Helper()
		pl, err := tu.db.Read.GetPlaylist(t.Context(), pid)
		if err != nil {
			t.Fatal(err)
		}
		return pl
	}

	// This tab loaded the page, and then another tab renamed the playlist.
	staleName := withSeen(t, tu, pid, url.Values{})
	mornings := "Mornings <b>"
	if err := playlist.UpdatePlaylist(t.Context(), tu.db, playlist.ByID(pid), playlist.Update{Name: &mornings}); err != nil {
		t.Fatal(err)
	}
	// This tab saves days, with the name it showed.
	typed := validPost()
	typed.Set("name", "Morning")
	for k, v := range staleName {
		typed[k] = v
	}
	w := tu.post(t, c, path, typed)
	body := w.Body.String()
	wantMsg := template.HTMLEscapeString(`This playlist changed after the page loaded, so nothing was saved. It is now "Mornings <b>" (Every day, all day). Save again to use the values below.`)
	if w.Code != http.StatusConflict || !strings.Contains(body, wantMsg) {
		t.Fatalf("stale name: status %d, body %s; want 409 with %q", w.Code, body, wantMsg)
	}
	if pl := stored(); pl.Name != mornings || pl.Days != playlist.AllDays || pl.WindowStart != 0 {
		t.Errorf("a refused save changed the playlist: %+v", pl)
	}
	// The form keeps what was typed, and its hidden fields hold what is stored.
	if !strings.Contains(body, `required value="Morning"`) || !strings.Contains(body, `value="08:00"`) || !strings.Contains(body, `value="tue" checked`) {
		t.Errorf("the refusal lost the typed values:\n%s", body)
	}
	if got := hidden(t, body, "seen_name"); got != mornings {
		t.Errorf("seen_name = %q, want the stored name", got)
	}
	if strings.Contains(body, "<b>") {
		t.Error("the refusal has an unescaped name")
	}
	// Saving again from the refusal's own fields applies the typed values.
	again := validPost()
	again.Set("name", "Morning")
	for _, name := range seenFields {
		again.Set(name, hidden(t, body, name))
	}
	if got := tu.post(t, c, path, again); got.Code != http.StatusSeeOther {
		t.Fatalf("save again: status %d: %s", got.Code, got.Body)
	}
	if pl := stored(); pl.Name != "Morning" || pl.WindowStart != 8*60 {
		t.Errorf("after saving again: %+v", pl)
	}

	// This tab loaded the page, and then another tab set a schedule.
	staleSchedule := withSeen(t, tu, pid, url.Values{})
	weekdays := playlist.Schedule{Days: 0b0111110, WindowStart: 9 * 60, WindowEnd: 17 * 60}.Full()
	if err := playlist.UpdatePlaylist(t.Context(), tu.db, playlist.ByID(pid), playlist.Update{Schedule: &weekdays}); err != nil {
		t.Fatal(err)
	}
	// This tab renames, with the schedule it showed.
	rename := validPost()
	rename.Set("name", "Breakfast")
	for k, v := range staleSchedule {
		rename[k] = v
	}
	w = tu.post(t, c, path, rename)
	body = w.Body.String()
	wantMsg = `It is now &#34;Morning&#34; (Weekdays, 09:00 to 17:00). Save again`
	if w.Code != http.StatusConflict || !strings.Contains(body, wantMsg) {
		t.Fatalf("stale schedule: status %d, body %s; want 409 with %q", w.Code, body, wantMsg)
	}
	if pl := stored(); pl.Name != "Morning" || pl.Days != 0b0111110 || pl.WindowStart != 9*60 || pl.WindowEnd != 17*60 {
		t.Errorf("a refused save changed the playlist: %+v", pl)
	}
	if got := hidden(t, body, "seen_days"); got != "62" {
		t.Errorf("seen_days = %q, want the stored 62", got)
	}
}

// TestPlaylistSaveWithoutSeen checks that a save that does not say what its
// page showed is refused with 400 and changes nothing.
func TestPlaylistSaveWithoutSeen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(v url.Values)
	}{
		{"no seen fields", func(v url.Values) {
			for _, name := range seenFields {
				v.Del(name)
			}
		}},
		{"no seen name", func(v url.Values) { v.Del("seen_name") }},
		{"no seen days", func(v url.Values) { v.Del("seen_days") }},
		{"no seen end", func(v url.Values) { v.Del("seen_ends_at") }},
		{"seen days not a number", func(v url.Values) { v.Set("seen_days", "all") }},
		{"seen start empty", func(v url.Values) { v.Set("seen_starts_at", "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addItem(t, tu, "p", "a", 0)
			pid := playlistID(t, tu, "p")
			form := withSeen(t, tu, pid, validPost())
			form.Set("name", "fresh")
			tc.set(form)
			w := tu.post(t, tu.signIn(t), "/playlists/"+id(pid), form)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Reload the page and try again.") {
				t.Fatalf("status %d, body %s; want 400 saying to reload", w.Code, w.Body)
			}
			pl, _ := tu.db.Read.GetPlaylist(t.Context(), pid)
			if pl.Name != "p" || pl.Days != playlist.AllDays || pl.WindowStart != 0 {
				t.Errorf("playlist %+v changed by a refused save", pl)
			}
		})
	}
}

// TestPlaylistDeleteStale posts the delete form from a confirm page that
// another change has made out of date, and checks that it is refused with
// 409, deletes nothing, and shows what goes with the playlist now.
func TestPlaylistDeleteStale(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, tu *testUI, pid int64)
		want   string // in the page that comes back.
	}{
		{"a device linked", func(t *testing.T, tu *testUI, pid int64) { linkTo(t, tu, "ABC123", pid) }, "Kitchen stops showing it."},
		{"an item added", func(t *testing.T, tu *testUI, pid int64) {
			if _, err := playlist.AddItem(t.Context(), tu.db, playlist.ByID(pid), "", addInstance(t, tu, "more"), 0); err != nil {
				t.Fatal(err)
			}
		}, "Its 1 item goes with it."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
			pid := newPlaylist(t, tu, "Lunch")
			c := tu.signIn(t)
			// The confirm page said it has no items and no device shows it.
			shown := withContents(t, tu, pid, url.Values{})
			if shown.Get("items") != "0" || shown.Get("devices") != "" {
				t.Fatalf("the confirm page holds %v, want no items and no devices", shown)
			}
			tc.change(t, tu, pid)
			w := tu.post(t, c, "/playlists/delete/"+id(pid), shown)
			body := w.Body.String()
			for _, want := range []string{"This playlist changed after the page loaded, so nothing was deleted. Check what goes with it now.", tc.want} {
				if !strings.Contains(body, want) {
					t.Errorf("the refusal lacks %q", want)
				}
			}
			if w.Code != http.StatusConflict {
				t.Fatalf("status %d, want 409", w.Code)
			}
			if names := playlistNames(t, tu); len(names) != 1 {
				t.Fatalf("playlists %v, want Lunch to still exist", names)
			}
			// The page holds the current values, so deleting from it goes through.
			again := url.Values{"items": {hidden(t, body, "items")}, "devices": {hidden(t, body, "devices")}}
			if w := tu.post(t, c, "/playlists/delete/"+id(pid), again); w.Code != http.StatusSeeOther {
				t.Fatalf("delete from the refusal's fields: status %d: %s", w.Code, w.Body)
			}
			if names := playlistNames(t, tu); len(names) != 0 {
				t.Errorf("playlists %v, want none", names)
			}
		})
	}
}

// TestPlaylistDeleteWithoutContents checks that a delete that does not say
// what its confirm page showed is refused with 400 and deletes nothing.
func TestPlaylistDeleteWithoutContents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{"no fields", url.Values{}},
		{"no items", url.Values{"devices": {""}}},
		{"no devices", url.Values{"items": {"0"}}},
		{"items not a number", url.Values{"items": {"none"}, "devices": {""}}},
		{"devices not numbers", url.Values{"items": {"0"}, "devices": {"1,x"}}},
		{"devices with a gap", url.Values{"items": {"0"}, "devices": {"1,,2"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			pid := newPlaylist(t, tu, "Lunch")
			w := tu.post(t, tu.signIn(t), "/playlists/delete/"+id(pid), tc.form)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Reload the page and try again.") {
				t.Fatalf("status %d, body %s; want 400 saying to reload", w.Code, w.Body)
			}
			if names := playlistNames(t, tu); len(names) != 1 {
				t.Errorf("playlists %v, want Lunch to still exist", names)
			}
		})
	}
}

func TestPlaylistDevicesPanel(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen <b>x</b>", 0)
	addDevice(t, tu, "D8:3B:DA:00:00:02", "DEF456", "", 0)
	addDevice(t, tu, "D8:3B:DA:00:00:03", "GHI789", "Hall", 0)
	addDevice(t, tu, "D8:3B:DA:00:00:04", "JKL012", "", 0)
	q, p, r, s := newPlaylist(t, tu, "q"), newPlaylist(t, tu, "p"), newPlaylist(t, tu, "r"), newPlaylist(t, tu, "s")
	linkTo(t, tu, "ABC123", q, p, r)
	linkTo(t, tu, "DEF456", p, s)
	linkTo(t, tu, "JKL012", s)
	// A device with one playlist has no note.
	addDevice(t, tu, "D8:3B:DA:00:00:05", "MNO345", "Den", 0)
	only := newPlaylist(t, tu, "only")
	linkTo(t, tu, "MNO345", only)
	c := tu.signIn(t)
	unlink := func(pid int64, friendly, label string) string {
		return `<form method="post" action="/playlists/unlink/` + id(pid) + `"><input type="hidden" name="device" value="` + friendly +
			`"><button class="btn btn-secondary btn-danger" type="submit" aria-label="Unlink ` + label + `">Unlink</button></form>`
	}
	for _, tc := range []struct {
		name   string
		pid    int64
		want   []string
		absent []string
	}{
		{
			"first of three, and a device with other playlists to link", q,
			[]string{
				`<h2 id="devices-title">Devices</h2>`, `<a class="item-name" href="/devices/ABC123">Kitchen &lt;b&gt;x&lt;/b&gt;</a>`, `Tried first of 3 playlists`,
				unlink(q, "ABC123", "Kitchen &lt;b&gt;x&lt;/b&gt;"), `<label for="link_device">Link a device</label>`,
				`<option value="GHI789">Hall</option>`, `<option value="DEF456">DEF456</option>`, `<option value="JKL012">JKL012</option>`,
				`It goes last in the device's order. Change the order on the device's page.`, `action="/playlists/link/` + id(q) + `"`,
			},
			[]string{`<option value="ABC123"`, "No device shows this playlist."},
		},
		{
			"second of three", p,
			[]string{`Tried 2nd of 3 playlists`, `Tried first of 2 playlists`, `<a class="item-name" href="/devices/DEF456">DEF456</a>`},
			[]string{`<option value="ABC123"`, `<option value="DEF456"`},
		},
		{"third of three", r, []string{`Tried 3rd of 3 playlists`}, nil},
		{"only playlist of its device", only, []string{`href="/devices/MNO345">Den</a>`, `<option value="ABC123">`}, []string{"Tried"}},
		{"second of two", s, []string{`Tried 2nd of 2 playlists`, `href="/devices/JKL012">JKL012</a>`}, []string{`<option value="JKL012"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := tu.get(t, c, "/playlists/"+id(tc.pid)).Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(body, bad) {
					t.Errorf("page has %q", bad)
				}
			}
			if strings.Contains(body, "<b>") {
				t.Error("page has an unescaped name")
			}
		})
	}
}

func TestPlaylistDevicesPanelEmpty(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		devices bool
		link    bool
		want    string
		form    bool // whether the page has the link form.
	}{
		{"no devices", false, false, "No devices have registered yet.", false},
		{"every device linked", true, true, "Every device is linked.", false},
		{"a device to link", true, false, `<option value="ABC123">ABC123</option>`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			pid := newPlaylist(t, tu, "p")
			if tc.devices {
				addDevice(t, tu, testMAC, "ABC123", "", 0)
			}
			if tc.link {
				linkTo(t, tu, "ABC123", pid)
			}
			body := tu.get(t, tu.signIn(t), "/playlists/"+id(pid)).Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("page lacks %q:\n%s", tc.want, body)
			}
			if got := strings.Contains(body, `action="/playlists/link/`); got != tc.form {
				t.Errorf("link form shown = %v, want %v", got, tc.form)
			}
		})
	}
}

func TestPlaylistDeviceLink(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "", 0)
	first, second := newPlaylist(t, tu, "first"), newPlaylist(t, tu, "second")
	linkTo(t, tu, "ABC123", first)
	c := tu.signIn(t)
	w := tu.post(t, c, "/playlists/link/"+id(second), url.Values{"device": {"ABC123"}})
	if want := "/playlists/" + id(second) + "#devices"; w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
		t.Fatalf("status %d to %q, want 303 to %q: %s", w.Code, w.Header().Get("Location"), want, w.Body)
	}
	if got := linkedIDs(t, tu, "ABC123"); len(got) != 2 || got[0] != first || got[1] != second {
		t.Errorf("links %v, want the new one last after %d", got, first)
	}
	// Linking again changes nothing, and the page shows it.
	if w := tu.post(t, c, "/playlists/link/"+id(second), url.Values{"device": {"ABC123"}}); w.Code != http.StatusSeeOther {
		t.Errorf("linking twice: status %d", w.Code)
	}
	if got := linkedIDs(t, tu, "ABC123"); len(got) != 2 {
		t.Errorf("links %v after linking twice", got)
	}
	if body := tu.get(t, c, "/playlists/"+id(second)).Body.String(); !strings.Contains(body, "Tried 2nd of 2 playlists") || !strings.Contains(body, "Every device is linked.") {
		t.Errorf("page after linking:\n%s", body)
	}
}

func TestPlaylistDeviceLinkRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"no device", url.Values{}, "Choose a device."},
		{"empty device", url.Values{"device": {"  "}}, "Choose a device."},
		{"unknown device", url.Values{"device": {"ZZZ999"}}, "That device no longer exists."},
		{"unknown MAC", url.Values{"device": {"D8:3B:DA:99:99:99"}}, "That device no longer exists."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			addDevice(t, tu, testMAC, "ABC123", "", 0)
			pid := newPlaylist(t, tu, "p")
			w := tu.post(t, tu.signIn(t), "/playlists/link/"+id(pid), tc.form)
			body := w.Body.String()
			if w.Code != http.StatusBadRequest || !strings.Contains(body, tc.want) || !strings.Contains(body, `role="alert"`) {
				t.Fatalf("status %d, body %s; want 400 with %q", w.Code, body, tc.want)
			}
			if !strings.Contains(body, "<h1>p</h1>") || !strings.Contains(body, `<option value="ABC123">`) {
				t.Error("the refusal does not show the playlist page")
			}
			if got := linkedIDs(t, tu, "ABC123"); len(got) != 0 {
				t.Errorf("links %v after a refused link", got)
			}
		})
	}
}

func TestPlaylistDeviceUnlink(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "", 0)
	a, b, c := newPlaylist(t, tu, "a"), newPlaylist(t, tu, "b"), newPlaylist(t, tu, "c")
	linkTo(t, tu, "ABC123", a, b, c)
	cookie := tu.signIn(t)
	want := "/playlists/" + id(b) + "#devices"
	// The second post finds the link gone, which is what it asked for.
	for i := range 2 {
		w := tu.post(t, cookie, "/playlists/unlink/"+id(b), url.Values{"device": {"ABC123"}})
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
			t.Fatalf("post %d: status %d to %q, want 303 to %q: %s", i+1, w.Code, w.Header().Get("Location"), want, w.Body)
		}
		if got := linkedIDs(t, tu, "ABC123"); len(got) != 2 || got[0] != a || got[1] != c {
			t.Errorf("post %d: links %v, want %d and %d", i+1, got, a, c)
		}
	}
	// The device's order closes up: c is second of two.
	if body := tu.get(t, cookie, "/playlists/"+id(c)).Body.String(); !strings.Contains(body, "Tried 2nd of 2 playlists") {
		t.Errorf("order after an unlink:\n%s", body)
	}
	// A device that is not linked, or that is gone, is refused or ignored without a failure.
	if w := tu.post(t, cookie, "/playlists/unlink/"+id(b), url.Values{}); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Choose a device.") {
		t.Errorf("no device: status %d", w.Code)
	}
	if w := tu.post(t, cookie, "/playlists/unlink/"+id(b), url.Values{"device": {"ZZZ999"}}); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "That device no longer exists.") {
		t.Errorf("unknown device: status %d", w.Code)
	}
}

func TestPlaylistDeleteConfirmSummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		items   int
		devices int
		want    string
	}{
		{"no items, no devices", 0, 0, "It has no items. No device shows it. The instances stay."},
		{"one item, one device", 1, 1, "Its 1 item goes with it. ABC123 stops showing it. The instances stay."},
		{"three items, two devices", 3, 2, "Its 3 items go with it. ABC123 and Kitchen &lt;b&gt; stop showing it. The instances stay."},
		{"two items, four devices", 2, 4, "Its 2 items go with it. 4 devices stop showing it. The instances stay."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			pid := newPlaylist(t, tu, "Rooms <i>")
			for i := range tc.items {
				if _, err := playlist.AddItem(t.Context(), tu.db, playlist.ByID(pid), "", addInstance(t, tu, "inst"+id(int64(i))), 0); err != nil {
					t.Fatal(err)
				}
			}
			friendly := []string{"ABC123", "DEF456", "GHI789", "JKL012"}
			for i := range tc.devices {
				name := ""
				if i == 1 {
					name = "Kitchen <b>"
				}
				addDevice(t, tu, "D8:3B:DA:00:00:0"+id(int64(i+1)), friendly[i], name, 0)
				linkTo(t, tu, friendly[i], pid)
			}
			path := "/playlists/delete/" + id(pid)
			w := tu.get(t, tu.signIn(t), path)
			body := w.Body.String()
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			for _, want := range []string{
				"<h1>Delete playlist</h1>", "Delete Rooms &lt;i&gt;?", tc.want,
				`<form method="post" action="` + path + `">`,
				`<button class="btn btn-primary btn-danger-solid" type="submit">Delete playlist</button>`,
				`<a class="btn btn-secondary" href="/playlists/` + id(pid) + `">Keep playlist</a>`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("confirm page lacks %q:\n%s", want, body)
				}
			}
			if strings.Contains(body, "<i>") || strings.Contains(body, "<b>") {
				t.Error("confirm page has unescaped names")
			}
			// Asking changes nothing.
			if names := playlistNames(t, tu); len(names) != 1 {
				t.Errorf("GET deleted the playlist: %v", names)
			}
			if got := len(listItemsOf(t, tu, "Rooms <i>")); got != tc.items {
				t.Errorf("GET left %d items, want %d", got, tc.items)
			}
			for i := range tc.devices {
				if got := linkedIDs(t, tu, friendly[i]); len(got) != 1 {
					t.Errorf("GET changed the links of %s: %v", friendly[i], got)
				}
			}
		})
	}
}

// listItemsOf returns the items of the named playlist in order.
func listItemsOf(t *testing.T, tu *testUI, name string) []db.ListPlaylistItemsRow {
	t.Helper()
	all, err := tu.db.Read.ListPlaylistItems(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []db.ListPlaylistItemsRow
	for _, it := range all {
		if it.Playlist == name {
			out = append(out, it)
		}
	}
	return out
}

func TestPlaylistDelete(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "", 0)
	item := addItem(t, tu, "gone", "a", 0)
	keep := newPlaylist(t, tu, "keep")
	linkTo(t, tu, "ABC123", item.PlaylistID, keep)
	w := tu.post(t, tu.signIn(t), "/playlists/delete/"+id(item.PlaylistID), withContents(t, tu, item.PlaylistID, url.Values{}))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/playlists" {
		t.Fatalf("status %d to %q, want 303 to /playlists: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	if names := playlistNames(t, tu); len(names) != 1 || names[0] != "keep" {
		t.Errorf("playlists %v, want only keep", names)
	}
	if got := len(listItemsOf(t, tu, "gone")); got != 0 {
		t.Errorf("%d items of the deleted playlist remain", got)
	}
	insts, err := tu.db.Read.ListInstances(t.Context())
	if err != nil || len(insts) != 1 {
		t.Errorf("%d instances after the delete, want the 1 it showed: %v", len(insts), err)
	}
	if got := linkedIDs(t, tu, "ABC123"); len(got) != 1 || got[0] != keep {
		t.Errorf("links %v, want only keep", got)
	}
	// Deleting it again finds nothing.
	if w := tu.post(t, tu.signIn(t), "/playlists/delete/"+id(item.PlaylistID), url.Values{}); w.Code != http.StatusNotFound {
		t.Errorf("second delete: status %d, want 404", w.Code)
	}
}

func TestOrderNote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		index, total int64
		want         string
	}{
		{0, 1, ""},
		{0, 2, "Tried first of 2 playlists"},
		{1, 2, "Tried 2nd of 2 playlists"},
		{2, 3, "Tried 3rd of 3 playlists"},
		{3, 5, "Tried 4th of 5 playlists"},
		{10, 12, "Tried 11th of 12 playlists"},
		{11, 13, "Tried 12th of 13 playlists"},
		{12, 14, "Tried 13th of 14 playlists"},
		{20, 30, "Tried 21st of 30 playlists"},
		{21, 30, "Tried 22nd of 30 playlists"},
	} {
		if got := orderNote(tc.index, tc.total); got != tc.want {
			t.Errorf("orderNote(%d, %d) = %q, want %q", tc.index, tc.total, got, tc.want)
		}
	}
}
