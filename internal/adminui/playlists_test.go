package adminui

import (
	"net/http"
	"net/url"
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
	item, _, err := playlist.AddItem(t.Context(), tu.db, list, "", addInstance(t, tu, instance), d)
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

func TestPlaylistsEmpty(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	body := tu.get(t, tu.signIn(t), "/playlists").Body.String()
	for _, want := range []string{"<h1>Playlists</h1>", "<h2>No playlists yet</h2>"} {
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
	if _, _, err := playlist.AddItem(ctx, tu.db, "", dev.FriendlyID, addInstance(t, tu, "Solo"), 0); err != nil {
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
	if err := playlist.LinkDevice(ctx, tu.db, playlist.ByID(pl.ID), other.FriendlyID, nil); err != nil {
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
		{"index past the end", "/move", url.Values{"index": {"9"}}, "Position 9 is not between 0 and 1."},
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

	form := validPost()
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
			form := validPost()
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
	form := validPost()
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
			w := tu.get(t, tu.signIn(t), tc.path)
			if tc.method == "POST" {
				w = tu.post(t, tu.signIn(t), tc.path, url.Values{"duration": {"600"}, "days": {"mon"}, "index": {"0"}, "hidden": {"1"}})
			}
			body := w.Body.String()
			if w.Code != http.StatusNotFound || !strings.Contains(body, tc.want) || !strings.Contains(body, `<nav class="nav"`) {
				t.Errorf("status %d, body %s; want a 404 page in the shell with %q", w.Code, body, tc.want)
			}
			if got := len(listItems(t, tu)); got != 1 {
				t.Errorf("%d items left, want 1", got)
			}
		})
	}
}

func TestPlaylistsNeedSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/playlists"},
		{"GET", "/playlists/1"},
		{"POST", "/playlists/1"},
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
			body := ""
			if tc.method == "POST" {
				body = "index=0&hidden=1"
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
		})
	}
}

func TestPlaylistsCrossOrigin(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	a, b := addItem(t, tu, "p", "a", 0), addInstance(t, tu, "b")
	pid := playlistID(t, tu, "p")
	c := tu.signIn(t)
	form := url.Values{"index": {"0"}, "hidden": {"1"}, "duration": {"600"}, "days": {"mon"}, "instance": {id(b)}}
	for _, path := range []string{
		"/playlists/" + id(pid), "/playlists/add-item/" + id(pid), "/playlists/items/" + id(a.ID),
		"/playlists/items/" + id(a.ID) + "/move", "/playlists/items/" + id(a.ID) + "/hidden", "/playlists/items/" + id(a.ID) + "/delete",
	} {
		if w := tu.post(t, c, path, form, "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden {
			t.Errorf("cross-origin post to %s: status %d, want 403", path, w.Code)
		}
	}
	rows := listItems(t, tu)
	pl, _ := tu.db.Read.GetPlaylist(t.Context(), pid)
	if len(rows) != 1 || rows[0].Hidden != 0 || rows[0].Duration != 0 || pl.Days != playlist.AllDays {
		t.Errorf("a cross-origin post changed the playlist: %+v, %+v", rows, pl)
	}
}

func TestPlaylistFormTooBig(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	item := addItem(t, tu, "p", "a", 0)
	big := url.Values{"duration": {strings.Repeat("1", maxFormBody)}}
	for _, path := range []string{"/playlists/items/" + id(item.ID), "/playlists/" + id(playlistID(t, tu, "p")), "/playlists/items/" + id(item.ID) + "/move"} {
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
	for _, path := range []string{"/playlists", "/playlists/" + id(playlistID(t, tu, "p")), "/playlists/items/" + id(item.ID), "/playlists/items/" + id(item.ID) + "/delete", "/playlists/99"} {
		body := tu.get(t, c, path).Body.String()
		for _, bad := range []string{"<script", "style=", "<style", " onclick=", " onsubmit="} {
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
