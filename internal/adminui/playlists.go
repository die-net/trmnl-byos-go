package adminui

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
)

// dayLayout is how a date is shown, such as "2 Jan 2006".
const dayLayout = "2 Jan 2006"

// dayLabels are the weekday names the schedule form shows, in the order of
// the Days bits, so a label's index is the bit and playlist.DayNames(1<<i)
// is its form value.
var dayLabels = [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// dayBox is a weekday checkbox of the schedule form.
type dayBox struct {
	// Value is the lowercase name the form posts, such as "mon".
	Value   string
	Label   string
	Checked bool
}

// scheduleForm is a schedule as the form shows it: the text of its fields.
type scheduleForm struct {
	Days []dayBox
	// WindowStart and WindowEnd are HH:MM, both "" for all day.
	WindowStart, WindowEnd string
	// From and Until are the first and last day it is active as YYYY-MM-DD
	// in the server's zone, or "" for no bound.
	From, Until string
}

// newScheduleForm returns the form that shows s in the server's zone.
func (s *Server) newScheduleForm(sc playlist.Schedule) scheduleForm {
	f := scheduleForm{Days: make([]dayBox, len(dayLabels))}
	for i, label := range dayLabels {
		f.Days[i] = dayBox{Value: playlist.DayNames(1 << i)[0], Label: label, Checked: sc.Days&(1<<i) != 0}
	}
	if sc.WindowStart != sc.WindowEnd {
		f.WindowStart, f.WindowEnd = playlist.FormatClock(sc.WindowStart), playlist.FormatClock(sc.WindowEnd)
	}
	f.From, f.Until = playlist.FormatDay(sc.StartsAt, s.zone), playlist.FormatLastDay(sc.EndsAt, s.zone)
	return f
}

// scheduleFromPost reads the schedule fields of a posted form. It returns
// the form as submitted, so a refused one can be shown again, and the edit
// that replaces a schedule with it, or an error wrapping errkind.ErrInvalid.
// The edit is checked as a whole where it is applied.
func (s *Server) scheduleFromPost(r *http.Request) (scheduleForm, *playlist.ScheduleEdit, error) {
	post := r.PostForm
	f := scheduleForm{
		Days:        make([]dayBox, len(dayLabels)),
		WindowStart: strings.TrimSpace(post.Get("window_start")),
		WindowEnd:   strings.TrimSpace(post.Get("window_end")),
		From:        strings.TrimSpace(post.Get("from")),
		Until:       strings.TrimSpace(post.Get("until")),
	}
	for i, label := range dayLabels {
		v := playlist.DayNames(1 << i)[0]
		f.Days[i] = dayBox{Value: v, Label: label, Checked: slices.Contains(post["days"], v)}
	}
	days, err := playlist.DaysFromNames(post["days"])
	if err != nil {
		return f, nil, err
	}
	var start, end int64
	switch {
	case f.WindowStart == "" && f.WindowEnd == "":
	case f.WindowStart == "" || f.WindowEnd == "":
		return f, nil, fmt.Errorf("%w: give both a start and an end for the window, or neither for all day", errkind.ErrInvalid)
	default:
		if start, err = playlist.ParseClock(f.WindowStart); err != nil {
			return f, nil, err
		}
		if end, err = playlist.ParseClock(f.WindowEnd); err != nil {
			return f, nil, err
		}
		if start == end {
			start, end = 0, 0
		}
	}
	startsAt, err := playlist.ParseDay(f.From, s.zone)
	if err != nil {
		return f, nil, err
	}
	endsAt, err := playlist.ParseLastDay(f.Until, s.zone)
	if err != nil {
		return f, nil, err
	}
	if startsAt != 0 && endsAt != 0 && startsAt >= endsAt {
		return f, nil, fmt.Errorf("%w: the first day is after the last day", errkind.ErrInvalid)
	}
	e := playlist.Schedule{Days: days, WindowStart: start, WindowEnd: end, StartsAt: startsAt, EndsAt: endsAt}.Full()
	return f, &e, nil
}

// parseSeconds reads a duration given as a whole number of seconds.
func parseSeconds(text string) (time.Duration, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: the duration must be a whole number of seconds", errkind.ErrInvalid)
	}
	return playlist.DurationFromSeconds(n)
}

// invalidText returns the message of an error wrapping errkind.ErrInvalid,
// as a sentence to show.
func invalidText(err error) string {
	return capitalize(strings.TrimPrefix(err.Error(), errkind.ErrInvalid.Error()+": ")) + "."
}

// playlistDeviceView is a device a playlist is linked to.
type playlistDeviceView struct {
	// Name is the device's name, or its friendly ID.
	Name string
	Href string
}

// itemView is an item as the list shows it.
type itemView struct {
	ID       int64
	Instance string
	Duration string
	// Schedule is the item's schedule in words, or "" for the default.
	Schedule string
	Hidden   bool
	// Up and Down are the index the item moves to, and CanUp and CanDown
	// whether it has one.
	Up, Down       int64
	CanUp, CanDown bool
	// EditHref is the path of the item's page, and the others are the
	// paths its small forms post to.
	EditHref, MoveHref, HideHref, DeleteHref string
}

// playlistView is a playlist as the list shows it.
type playlistView struct {
	ID   int64
	Name string
	// Schedule is the playlist's schedule in words.
	Schedule string
	Href     string
	Devices  []playlistDeviceView
	Items    []itemView
}

// itemForm is the item form's values.
type itemForm struct {
	// Duration is the number of seconds as text.
	Duration string
	Hidden   bool
	Schedule scheduleForm
}

// itemEdit is an item as its page shows it.
type itemEdit struct {
	// Action is the path the form posts to.
	Action string
	// Instance is the name of the instance the item shows, and Playlist the
	// name of its playlist.
	Instance string
	Playlist string
	// MinSeconds is the shortest duration other than 0.
	MinSeconds int64
	Form       itemForm
}

// instanceOption is a choice in the instance list.
type instanceOption struct {
	ID       int64
	Label    string
	Selected bool
}

// addForm is the add item form's values.
type addForm struct {
	Instance int64
	// Duration is the number of seconds as text.
	Duration string
}

// playlistEdit is a playlist as its page shows it.
type playlistEdit struct {
	Name string
	// Action and AddAction are the paths the schedule and add item forms
	// post to.
	Action, AddAction string
	Schedule          scheduleForm
	Instances         []instanceOption
	Add               addForm
	MinSeconds        int64
}

// notFoundView says what the not-found page could not find.
type notFoundView struct {
	// Nav is the nav destination the page belongs to, such as "devices".
	Nav string
	// What is the heading, such as "No such device".
	What string
	// BackHref and BackName are the page the link goes back to.
	BackHref, BackName string
}

// renderNotFound sends the not-found page with a 404.
func (s *Server) renderNotFound(w http.ResponseWriter, r *http.Request, nf notFoundView) {
	s.render(w, r, http.StatusNotFound, "notfound", pageData{Nav: nf.Nav, Heading: "Not found", NotFound: nf})
}

var (
	noPlaylist = notFoundView{Nav: "playlists", What: "No such playlist", BackHref: "/playlists", BackName: "Playlists"}
	noItem     = notFoundView{Nav: "playlists", What: "No such playlist item", BackHref: "/playlists", BackName: "Playlists"}
)

// serverError logs err and sends a generic 500. A request whose context has
// ended, because the client hung up or its deadline passed, has no one to
// answer and has not failed on the server's side: the error is the
// context's, so it is logged for debugging only and nothing is written.
func serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		slog.Debug("admin ui: "+what+": request ended", "err", err)
		return
	}
	slog.Error("admin ui: "+what, "err", err)
	http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
}

// idFromPath returns the numeric {id} of the route. A path that is not a
// number names nothing.
func idFromPath(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

// playlistViews describes every playlist with its devices and items, from
// three list queries however many there are, read in one snapshot.
func (s *Server) playlistViews(ctx context.Context) ([]playlistView, error) {
	var views []playlistView
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		lists, err := q.ListPlaylists(ctx)
		if err != nil {
			return fmt.Errorf("list playlists: %w", err)
		}
		devs, err := q.ListPlaylistDevices(ctx)
		if err != nil {
			return fmt.Errorf("list playlist devices: %w", err)
		}
		items, err := q.ListPlaylistItems(ctx)
		if err != nil {
			return fmt.Errorf("list playlist items: %w", err)
		}
		views = s.buildPlaylistViews(lists, devs, items)
		return nil
	})
	return views, err
}

// buildPlaylistViews describes the playlists, each with the device and item
// rows that name it by ID. A row whose playlist is not in lists is skipped.
func (s *Server) buildPlaylistViews(lists []db.Playlist, devs []db.ListPlaylistDevicesRow, items []db.ListPlaylistItemsRow) []playlistView {
	out := make([]playlistView, len(lists))
	byID := make(map[int64]*playlistView, len(lists))
	for i, l := range lists {
		sc := playlist.Schedule{Days: l.Days, WindowStart: l.WindowStart, WindowEnd: l.WindowEnd, StartsAt: l.StartsAt, EndsAt: l.EndsAt}
		out[i] = playlistView{ID: l.ID, Name: l.Name, Schedule: s.describeSchedule(sc), Href: "/playlists/" + strconv.FormatInt(l.ID, 10)}
		byID[l.ID] = &out[i]
	}
	for _, d := range devs {
		name := d.DeviceName
		if name == "" {
			name = d.FriendlyID
		}
		if p := byID[d.PlaylistID]; p != nil {
			p.Devices = append(p.Devices, playlistDeviceView{Name: name, Href: "/devices/" + d.FriendlyID})
		}
	}
	for _, it := range items {
		p := byID[it.PlaylistID]
		if p == nil {
			continue
		}
		sc := playlist.Schedule{Days: it.Days, WindowStart: it.WindowStart, WindowEnd: it.WindowEnd, StartsAt: it.StartsAt, EndsAt: it.EndsAt}
		v := itemView{ID: it.ID, Instance: it.InstanceName, Duration: itemDuration(it.Duration), Hidden: it.Hidden != 0}
		if sc != playlist.Always {
			v.Schedule = s.describeSchedule(sc)
		}
		base := "/playlists/items/" + strconv.FormatInt(it.ID, 10)
		v.EditHref, v.MoveHref, v.HideHref, v.DeleteHref = base, base+"/move", base+"/hidden", base+"/delete"
		v.Up, v.Down = int64(len(p.Items))-1, int64(len(p.Items))+1
		v.CanUp = len(p.Items) > 0
		p.Items = append(p.Items, v)
	}
	for i := range out {
		for j := range out[i].Items {
			out[i].Items[j].CanDown = j < len(out[i].Items)-1
		}
	}
	return out
}

func (s *Server) playlists(w http.ResponseWriter, r *http.Request) {
	s.renderPlaylists(w, r, http.StatusOK, "")
}

// renderPlaylists sends the playlists page, with an error message above the
// list if msg is not "".
func (s *Server) renderPlaylists(w http.ResponseWriter, r *http.Request, status int, msg string) {
	views, err := s.playlistViews(r.Context())
	if err != nil {
		serverError(w, r, "list playlists", err)
		return
	}
	s.render(w, r, status, "playlists", pageData{Nav: "playlists", Heading: "Playlists", Error: msg, Playlists: views})
}

// itemAction returns the handler of a small form that changes one item and
// goes back to its place on the playlists page. do makes the change from the
// posted form. An item that does not exist gets the not-found page, and a
// refused change the list with the reason.
func (s *Server) itemAction(do func(ctx context.Context, post map[string][]string, id int64) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idFromPath(r)
		if !ok {
			s.renderNotFound(w, r, noItem)
			return
		}
		item, err := s.db.Read.GetPlaylistItem(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			s.renderNotFound(w, r, noItem)
			return
		} else if err != nil {
			serverError(w, r, "get playlist item", err)
			return
		}
		if !parseForm(w, r, func(status int, msg string) { s.renderPlaylists(w, r, status, msg) }) {
			return
		}
		err = do(r.Context(), r.PostForm, id)
		switch {
		case err == nil:
			http.Redirect(w, r, "/playlists#playlist-"+strconv.FormatInt(item.PlaylistID, 10), http.StatusSeeOther)
		case errors.Is(err, playlist.ErrNoItem):
			s.renderNotFound(w, r, noItem)
		case errors.Is(err, errkind.ErrInvalid):
			s.renderPlaylists(w, r, http.StatusBadRequest, invalidText(err))
		default:
			serverError(w, r, "change playlist item", err)
		}
	}
}

// moveItem moves an item to the index the form names.
func (s *Server) moveItem(ctx context.Context, post map[string][]string, id int64) error {
	var index string
	if v := post["index"]; len(v) > 0 {
		index = v[0]
	}
	n, err := strconv.ParseInt(index, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %q is not a position", errkind.ErrInvalid, index)
	}
	return playlist.MoveItem(ctx, s.db, id, n)
}

// hideItem hides or shows an item, as the form's hidden field says.
func (s *Server) hideItem(ctx context.Context, post map[string][]string, id int64) error {
	var hidden bool
	switch v := post["hidden"]; {
	case len(v) == 1 && v[0] == "1":
		hidden = true
	case len(v) == 1 && v[0] == "0":
	default:
		return fmt.Errorf("%w: choose Hide or Show", errkind.ErrInvalid)
	}
	return playlist.UpdateItem(ctx, s.db, id, playlist.ItemUpdate{Hidden: &hidden})
}

// deleteItem removes an item.
func (s *Server) deleteItem(ctx context.Context, _ map[string][]string, id int64) error {
	return playlist.DeleteItem(ctx, s.db, id)
}

// fromPath returns what the numeric {id} of the route names, which get
// reads. A path that is not a number, or an ID with no row, gets the
// not-found page nf, and any other failure a plain error, and both return false.
func fromPath[T any](s *Server, w http.ResponseWriter, r *http.Request, what string, nf notFoundView, get func(ctx context.Context, id int64) (T, error)) (T, bool) {
	var zero T
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, nf)
		return zero, false
	}
	row, err := get(r.Context(), id)
	switch {
	case err == nil:
		return row, true
	case errors.Is(err, sql.ErrNoRows):
		s.renderNotFound(w, r, nf)
	default:
		serverError(w, r, what, err)
	}
	return zero, false
}

// itemFromPath returns the item the path names with its playlist and
// instance.
func (s *Server) itemFromPath(w http.ResponseWriter, r *http.Request) (db.GetPlaylistItemViewRow, bool) {
	return fromPath(s, w, r, "get playlist item", noItem, s.db.Read.GetPlaylistItemView)
}

func (s *Server) itemPage(w http.ResponseWriter, r *http.Request) {
	row, ok := s.itemFromPath(w, r)
	if !ok {
		return
	}
	it := &row.PlaylistItem
	sc := playlist.Schedule{Days: it.Days, WindowStart: it.WindowStart, WindowEnd: it.WindowEnd, StartsAt: it.StartsAt, EndsAt: it.EndsAt}
	s.renderItem(w, r, http.StatusOK, &row, &itemForm{Duration: strconv.FormatInt(it.Duration, 10), Hidden: it.Hidden != 0, Schedule: s.newScheduleForm(sc)}, "")
}

// renderItem sends the page of an item with the form values, and an error
// message above the form if msg is not "".
func (s *Server) renderItem(w http.ResponseWriter, r *http.Request, status int, row *db.GetPlaylistItemViewRow, form *itemForm, msg string) {
	e := &itemEdit{
		Action:     "/playlists/items/" + strconv.FormatInt(row.PlaylistItem.ID, 10),
		Instance:   row.InstanceName,
		Playlist:   row.PlaylistName,
		MinSeconds: int64(playlist.MinDuration / time.Second),
		Form:       *form,
	}
	s.render(w, r, status, "playlist_item", pageData{Nav: "playlists", Heading: row.InstanceName, Error: msg, ItemEdit: e})
}

// saveItem applies the item form in one update, so a bad field changes
// nothing.
func (s *Server) saveItem(w http.ResponseWriter, r *http.Request) {
	row, ok := s.itemFromPath(w, r)
	if !ok {
		return
	}
	it := &row.PlaylistItem
	form := itemForm{
		Duration: strconv.FormatInt(it.Duration, 10), Hidden: it.Hidden != 0,
		Schedule: s.newScheduleForm(playlist.Schedule{Days: it.Days, WindowStart: it.WindowStart, WindowEnd: it.WindowEnd, StartsAt: it.StartsAt, EndsAt: it.EndsAt}),
	}
	if !parseForm(w, r, func(status int, msg string) { s.renderItem(w, r, status, &row, &form, msg) }) {
		return
	}
	form.Duration = strings.TrimSpace(r.PostForm.Get("duration"))
	form.Hidden = r.PostForm.Get("hidden") != ""
	var err error
	var edit *playlist.ScheduleEdit
	form.Schedule, edit, err = s.scheduleFromPost(r)
	var dur time.Duration
	if err == nil {
		dur, err = parseSeconds(form.Duration)
	}
	if err == nil {
		err = playlist.UpdateItem(r.Context(), s.db, row.PlaylistItem.ID, playlist.ItemUpdate{Duration: &dur, Hidden: &form.Hidden, Schedule: edit})
	}
	switch {
	case err == nil:
		http.Redirect(w, r, "/playlists#playlist-"+strconv.FormatInt(row.PlaylistItem.PlaylistID, 10), http.StatusSeeOther)
	case errors.Is(err, playlist.ErrNoItem):
		s.renderNotFound(w, r, noItem)
	case errors.Is(err, errkind.ErrInvalid):
		s.renderItem(w, r, http.StatusBadRequest, &row, &form, invalidText(err))
	default:
		serverError(w, r, "save playlist item", err)
	}
}

// playlistFromPath returns the playlist the path names.
func (s *Server) playlistFromPath(w http.ResponseWriter, r *http.Request) (db.Playlist, bool) {
	return fromPath(s, w, r, "get playlist", noPlaylist, s.db.Read.GetPlaylist)
}

func (s *Server) playlistPage(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromPath(w, r)
	if !ok {
		return
	}
	sc := scheduleOf(&pl)
	f := s.newScheduleForm(sc)
	s.renderPlaylist(w, r, http.StatusOK, &pl, &f, &addForm{Duration: strconv.FormatInt(int64(playlist.DefaultDuration/time.Second), 10)}, "")
}

// scheduleOf returns a playlist's schedule.
func scheduleOf(pl *db.Playlist) playlist.Schedule {
	return playlist.Schedule{Days: pl.Days, WindowStart: pl.WindowStart, WindowEnd: pl.WindowEnd, StartsAt: pl.StartsAt, EndsAt: pl.EndsAt}
}

// renderPlaylist sends the page of a playlist with the values of its two
// forms, and an error message above them if msg is not "".
func (s *Server) renderPlaylist(w http.ResponseWriter, r *http.Request, status int, pl *db.Playlist, sched *scheduleForm, add *addForm, msg string) {
	insts, err := s.db.Read.ListInstances(r.Context())
	if err != nil {
		serverError(w, r, "list instances", err)
		return
	}
	base := "/playlists/" + strconv.FormatInt(pl.ID, 10)
	e := &playlistEdit{
		Name: pl.Name, Action: base, AddAction: "/playlists/add-item/" + strconv.FormatInt(pl.ID, 10),
		Schedule: *sched, Add: *add, MinSeconds: int64(playlist.MinDuration / time.Second),
	}
	for i := range insts {
		inst := &insts[i].PluginInstance
		e.Instances = append(e.Instances, instanceOption{ID: inst.ID, Label: inst.Name + " (" + insts[i].RecipeName + ")", Selected: inst.ID == add.Instance})
	}
	s.render(w, r, status, "playlist", pageData{Nav: "playlists", Heading: pl.Name, Error: msg, PlaylistEdit: e})
}

// savePlaylist applies the schedule form to a playlist.
func (s *Server) savePlaylist(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromPath(w, r)
	if !ok {
		return
	}
	sched := s.newScheduleForm(scheduleOf(&pl))
	add := &addForm{Duration: strconv.FormatInt(int64(playlist.DefaultDuration/time.Second), 10)}
	if !parseForm(w, r, func(status int, msg string) { s.renderPlaylist(w, r, status, &pl, &sched, add, msg) }) {
		return
	}
	sched, edit, err := s.scheduleFromPost(r)
	if err == nil {
		err = playlist.UpdatePlaylist(r.Context(), s.db, playlist.ByID(pl.ID), playlist.Update{Schedule: edit})
	}
	switch {
	case err == nil:
		http.Redirect(w, r, "/playlists#playlist-"+strconv.FormatInt(pl.ID, 10), http.StatusSeeOther)
	case errors.Is(err, playlist.ErrNoPlaylist):
		s.renderNotFound(w, r, noPlaylist)
	case errors.Is(err, errkind.ErrInvalid):
		s.renderPlaylist(w, r, http.StatusBadRequest, &pl, &sched, add, invalidText(err))
	default:
		serverError(w, r, "save playlist", err)
	}
}

// addPlaylistItem appends an item for the instance and duration the add form
// names.
func (s *Server) addPlaylistItem(w http.ResponseWriter, r *http.Request) {
	pl, ok := s.playlistFromPath(w, r)
	if !ok {
		return
	}
	sched := s.newScheduleForm(scheduleOf(&pl))
	add := &addForm{}
	if !parseForm(w, r, func(status int, msg string) { s.renderPlaylist(w, r, status, &pl, &sched, add, msg) }) {
		return
	}
	add.Duration = strings.TrimSpace(r.PostForm.Get("duration"))
	var dur time.Duration
	instance, err := strconv.ParseInt(r.PostForm.Get("instance"), 10, 64)
	if err != nil {
		err = fmt.Errorf("%w: choose an instance", errkind.ErrInvalid)
	} else {
		add.Instance = instance
		dur, err = parseSeconds(add.Duration)
	}
	if err == nil {
		_, _, err = playlist.AddItem(r.Context(), s.db, pl.Name, "", instance, dur)
	}
	switch {
	case err == nil:
		http.Redirect(w, r, "/playlists#playlist-"+strconv.FormatInt(pl.ID, 10), http.StatusSeeOther)
	case errors.Is(err, errkind.ErrInvalid):
		s.renderPlaylist(w, r, http.StatusBadRequest, &pl, &sched, add, invalidText(err))
	case errors.Is(err, errkind.ErrNotFound):
		// The instance was removed after the page was shown.
		s.renderPlaylist(w, r, http.StatusBadRequest, &pl, &sched, add, "That instance no longer exists.")
	default:
		serverError(w, r, "add playlist item", err)
	}
}

// describeSchedule says when a schedule is active in words, such as
// "Weekdays, 06:00 to 09:00, until 24 Dec 2026". Dates are days in the
// server's zone, and the last day is the one the schedule includes.
func (s *Server) describeSchedule(sc playlist.Schedule) string {
	parts := []string{describeDays(sc.Days)}
	switch {
	case sc.WindowStart == sc.WindowEnd:
		parts = append(parts, "all day")
	case sc.WindowEnd < sc.WindowStart:
		parts = append(parts, playlist.FormatClock(sc.WindowStart)+" to "+playlist.FormatClock(sc.WindowEnd)+" the next day")
	default:
		parts = append(parts, playlist.FormatClock(sc.WindowStart)+" to "+playlist.FormatClock(sc.WindowEnd))
	}
	var from, until string
	if sc.StartsAt != 0 {
		from = time.Unix(sc.StartsAt, 0).In(s.zone).Format(dayLayout)
	}
	if sc.EndsAt != 0 {
		until = time.Unix(sc.EndsAt-1, 0).In(s.zone).Format(dayLayout)
	}
	switch {
	case from != "" && until != "":
		parts = append(parts, from+" to "+until)
	case from != "":
		parts = append(parts, "from "+from)
	case until != "":
		parts = append(parts, "until "+until)
	}
	return strings.Join(parts, ", ")
}

// describeDays says which weekdays a Days bitmask has: "Every day",
// "Weekdays", "Weekends", a run of three or more such as "Mon to Thu", or a
// list such as "Mon, Wed, Fri".
func describeDays(days int64) string {
	const weekdays, weekends = 0b0111110, 0b1000001
	switch days {
	case playlist.AllDays:
		return "Every day"
	case weekdays:
		return "Weekdays"
	case weekends:
		return "Weekends"
	}
	first, last := -1, -1
	var list []string
	for i, label := range dayLabels {
		if days&(1<<i) == 0 {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
		list = append(list, label)
	}
	if last-first+1 == len(list) && len(list) >= 3 {
		return dayLabels[first] + " to " + dayLabels[last]
	}
	return strings.Join(list, ", ")
}

// deleteView is the page that asks to confirm removing an item.
type deleteView struct {
	// Action is the path the Remove button posts to, and Cancel the page the
	// link goes back to.
	Action, Cancel     string
	Instance, Playlist string
}

// confirmDelete asks whether to remove an item. It changes nothing.
func (s *Server) confirmDelete(w http.ResponseWriter, r *http.Request) {
	row, ok := s.itemFromPath(w, r)
	if !ok {
		return
	}
	item := strconv.FormatInt(row.PlaylistItem.ID, 10)
	d := &deleteView{
		Action:   "/playlists/items/" + item + "/delete",
		Cancel:   "/playlists#playlist-" + strconv.FormatInt(row.PlaylistItem.PlaylistID, 10),
		Instance: row.InstanceName, Playlist: row.PlaylistName,
	}
	s.render(w, r, http.StatusOK, "playlist_delete", pageData{Nav: "playlists", Heading: "Remove item", Delete: d})
}
