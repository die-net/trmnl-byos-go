package adminui

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
)

// idList reads the comma-separated whole numbers in the post's field key,
// such as "3,1,2". An empty value is an empty list. It returns false if the
// field is missing or an element is not a whole number.
func idList(post url.Values, key string) ([]int64, bool) {
	if !post.Has(key) {
		return nil, false
	}
	value := strings.TrimSpace(post.Get(key))
	if value == "" {
		return []int64{}, true
	}
	parts := strings.Split(value, ",")
	ids := make([]int64, len(parts))
	for i, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			return nil, false
		}
		ids[i] = id
	}
	return ids, true
}

// joinIDs is the field value idList reads: the IDs, comma-separated.
func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

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

// kindText returns the message of an error that wraps one of the errkind
// kinds, as a sentence to show. A message that starts with the kind's own
// text, as one wrapped with fmt.Errorf("%w: ...", errkind.ErrInvalid) does,
// is shown without it.
func kindText(err error) string {
	msg := err.Error()
	for _, kind := range []error{errkind.ErrInvalid, errkind.ErrNotFound, errkind.ErrConflict, errkind.ErrTooBig} {
		if rest, ok := strings.CutPrefix(msg, kind.Error()+": "); ok {
			msg = rest
			break
		}
	}
	return sentence(msg)
}

// sentence returns msg as a sentence: its first letter in upper case, and
// one full stop at its end.
func sentence(msg string) string {
	return capitalize(strings.TrimSuffix(msg, ".")) + "."
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

// playlistForm is the values of the name and schedule form.
type playlistForm struct {
	Name     string
	Schedule scheduleForm
	// Seen is the name and schedule the page the form is on showed, which
	// the form sends back as hidden fields so a save from a page that is out
	// of date is refused. Nil means the playlist's own, as it is now.
	Seen *playlist.Seen
	// Changed is whether the save was refused because the playlist changed
	// after the page loaded. The page then says what it is now and holds its
	// current values in Seen, so saving again applies the typed ones.
	Changed bool
}

// linkedDevice is a device linked to the playlist a page shows.
type linkedDevice struct {
	// Name is the device's name, or its friendly ID.
	Name string
	// ID is the friendly ID, which the unlink form posts, and Href the path
	// of the device's page.
	ID, Href string
	// Note says where the playlist is in the device's order, or "" when it
	// is the device's only playlist.
	Note string
}

// deviceOption is a choice in the list of devices to link.
type deviceOption struct {
	// ID is the friendly ID the link form posts, and Label the device's
	// name, or its friendly ID.
	ID, Label string
}

// playlistEdit is a playlist as its page shows it.
type playlistEdit struct {
	// Action, AddAction, LinkAction and UnlinkAction are the paths the
	// forms post to, and DeleteHref the page that confirms a delete.
	Action, AddAction, LinkAction, UnlinkAction, DeleteHref string
	Form                                                    playlistForm
	// Devices are the devices linked to the playlist, and Unlinked those
	// that are not. NoDevices is whether no device is registered at all.
	Devices    []linkedDevice
	Unlinked   []deviceOption
	NoDevices  bool
	Instances  []instanceOption
	Add        addForm
	MinSeconds int64
}

// newPlaylistForm is the form that makes a playlist, on the playlists page.
type newPlaylistForm struct {
	// Name is what was typed, and Error why it was refused, or "".
	Name, Error string
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
	s.render(w, r, http.StatusNotFound, "notfound", &pageData{Nav: nf.Nav, Heading: "Not found", NotFound: nf})
}

var (
	noPlaylist = notFoundView{Nav: "playlists", What: "No such playlist", BackHref: "/playlists", BackName: "Playlists"}
	noItem     = notFoundView{Nav: "playlists", What: "No such playlist item", BackHref: "/playlists", BackName: "Playlists"}
)

// requestEnded reports whether err is the end of r's context, because the
// client hung up. That request has no one to answer and has not failed on the
// server's side, so it is logged for debugging only, and the caller writes
// nothing.
func requestEnded(r *http.Request, what string, err error) bool {
	if !errkind.Ended(r.Context(), err) {
		return false
	}
	slog.DebugContext(r.Context(), "admin ui: "+what+": request ended", "err", err)
	return true
}

// serverError logs err and sends a generic 500, unless the request ended.
func serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if requestEnded(r, what, err) {
		return
	}
	slog.ErrorContext(r.Context(), "admin ui: "+what, "err", err)
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
	s.renderPlaylists(w, r, http.StatusOK, "", newPlaylistForm{})
}

// renderPlaylists sends the playlists page, with an error message above the
// list if msg is not "", and the new playlist form as given.
func (s *Server) renderPlaylists(w http.ResponseWriter, r *http.Request, status int, msg string, create newPlaylistForm) {
	views, err := s.playlistViews(r.Context())
	if err != nil {
		serverError(w, r, "list playlists", err)
		return
	}
	s.render(w, r, status, "playlists", &pageData{Nav: "playlists", Heading: "Playlists", Error: msg, Playlists: views, NewPlaylist: create})
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
		if !parseForm(w, r, func(status int, msg string) { s.renderPlaylists(w, r, status, msg, newPlaylistForm{}) }) {
			return
		}
		err = do(r.Context(), r.PostForm, id)
		switch {
		case err == nil:
			http.Redirect(w, r, "/playlists#playlist-"+strconv.FormatInt(item.PlaylistID, 10), http.StatusSeeOther)
		case errors.Is(err, playlist.ErrNoItem):
			s.renderNotFound(w, r, noItem)
		case errors.Is(err, errkind.ErrInvalid):
			s.renderPlaylists(w, r, http.StatusBadRequest, kindText(err), newPlaylistForm{})
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
	s.render(w, r, status, "playlist_item", &pageData{Nav: "playlists", Heading: row.InstanceName, Error: msg, ItemEdit: e})
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
		s.renderItem(w, r, http.StatusBadRequest, &row, &form, kindText(err))
	default:
		serverError(w, r, "save playlist item", err)
	}
}

func (s *Server) playlistPage(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, noPlaylist)
		return
	}
	s.renderPlaylist(w, r, http.StatusOK, id, nil, nil, "")
}

// scheduleOf returns a playlist's schedule.
func scheduleOf(pl *db.Playlist) playlist.Schedule {
	return playlist.Schedule{Days: pl.Days, WindowStart: pl.WindowStart, WindowEnd: pl.WindowEnd, StartsAt: pl.StartsAt, EndsAt: pl.EndsAt}
}

// renderPlaylist sends the page of the playlist with the ID, which gets the
// not-found page if it does not exist. form and add are the values of the
// name and schedule form and of the add item form, and nil shows the stored
// name and schedule, and the default duration. msg is an error message
// shown above the forms, or "". The heading is always the stored name. Its
// reads are one snapshot, with a fixed number of queries.
func (s *Server) renderPlaylist(w http.ResponseWriter, r *http.Request, status int, id int64, form *playlistForm, add *addForm, msg string) {
	ctx := r.Context()
	var pl db.Playlist
	var links []db.ListPlaylistDevicesRow
	var devs []db.ListDeviceNamesRow
	var insts []db.ListInstancesRow
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		if pl, err = q.GetPlaylist(ctx, id); err != nil {
			return fmt.Errorf("get playlist: %w", err)
		}
		if links, err = q.ListPlaylistDevices(ctx); err != nil {
			return fmt.Errorf("list playlist devices: %w", err)
		}
		if devs, err = q.ListDeviceNames(ctx); err != nil {
			return fmt.Errorf("list devices: %w", err)
		}
		if insts, err = q.ListInstances(ctx); err != nil {
			return fmt.Errorf("list instances: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.renderNotFound(w, r, noPlaylist)
		return
	case err != nil:
		serverError(w, r, "read playlist page", err)
		return
	}
	if form == nil {
		form = &playlistForm{Name: pl.Name, Schedule: s.newScheduleForm(scheduleOf(&pl))}
	}
	if add == nil {
		add = &addForm{Duration: strconv.FormatInt(int64(playlist.DefaultDuration/time.Second), 10)}
	}
	if form.Seen == nil || form.Changed {
		f := *form
		f.Seen = &playlist.Seen{Name: pl.Name, Schedule: scheduleOf(&pl)}
		form = &f
	}
	if form.Changed {
		msg = "This playlist changed after the page loaded, so nothing was saved. It is now \"" + pl.Name + "\" (" +
			s.describeSchedule(form.Seen.Schedule) + "). Save again to use the values below."
	}
	base := strconv.FormatInt(pl.ID, 10)
	e := &playlistEdit{
		Action: "/playlists/" + base, AddAction: "/playlists/add-item/" + base,
		LinkAction: "/playlists/link/" + base, UnlinkAction: "/playlists/unlink/" + base, DeleteHref: "/playlists/delete/" + base,
		Form: *form, Add: *add, MinSeconds: int64(playlist.MinDuration / time.Second),
		NoDevices: len(devs) == 0,
	}
	e.Devices, e.Unlinked = linkedDevices(pl.ID, links, devs)
	for i := range insts {
		inst := &insts[i].PluginInstance
		e.Instances = append(e.Instances, instanceOption{ID: inst.ID, Label: inst.Name + " (" + insts[i].RecipeName + ")", Selected: inst.ID == add.Instance})
	}
	s.render(w, r, status, "playlist", &pageData{Nav: "playlists", Heading: pl.Name, Error: msg, PlaylistEdit: e})
}

// deviceLabel returns a device's name, or its friendly ID if it has none.
func deviceLabel(name, friendlyID string) string {
	if name == "" {
		return friendlyID
	}
	return name
}

// linkedDevices splits the devices into those linked to the playlist, each
// with where the playlist is in its order, and those that are not. links is
// every link, which gives how many playlists each device has, and devs
// every device.
func linkedDevices(playlistID int64, links []db.ListPlaylistDevicesRow, devs []db.ListDeviceNamesRow) ([]linkedDevice, []deviceOption) {
	total := map[int64]int64{}
	for i := range links {
		total[links[i].DeviceID]++
	}
	var linked []linkedDevice
	isLinked := map[int64]bool{}
	for i := range links {
		l := &links[i]
		if l.PlaylistID != playlistID {
			continue
		}
		isLinked[l.DeviceID] = true
		linked = append(linked, linkedDevice{
			Name: deviceLabel(l.DeviceName, l.FriendlyID), ID: l.FriendlyID, Href: "/devices/" + l.FriendlyID,
			Note: orderNote(l.Priority, total[l.DeviceID]),
		})
	}
	var unlinked []deviceOption
	for i := range devs {
		if !isLinked[devs[i].ID] {
			unlinked = append(unlinked, deviceOption{ID: devs[i].FriendlyID, Label: deviceLabel(devs[i].Name, devs[i].FriendlyID)})
		}
	}
	return linked, unlinked
}

// orderNote says where a playlist, at index in a device's order of total
// playlists, is tried, such as "Tried 2nd of 3 playlists". It is "" for a
// device with only that playlist.
func orderNote(index, total int64) string {
	switch {
	case total < 2:
		return ""
	case index == 0:
		return fmt.Sprintf("Tried first of %d playlists", total)
	default:
		return fmt.Sprintf("Tried %s of %d playlists", ordinal(index+1), total)
	}
}

// ordinal returns n with its suffix, such as "2nd" or "12th".
func ordinal(n int64) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.FormatInt(n, 10) + suffix
}

// savePlaylist applies the name and schedule form to a playlist in one
// update, so a refused form changes nothing.
func (s *Server) savePlaylist(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, noPlaylist)
		return
	}
	if !parseForm(w, r, func(status int, msg string) { s.renderPlaylist(w, r, status, id, nil, nil, msg) }) {
		return
	}
	seen, ok := seenFromPost(r.PostForm)
	if !ok {
		s.renderPlaylist(w, r, http.StatusBadRequest, id, nil, nil, "Reload the page and try again.")
		return
	}
	name := r.PostForm.Get("name")
	form := playlistForm{Name: name, Seen: &seen}
	var edit *playlist.ScheduleEdit
	var err error
	form.Schedule, edit, err = s.scheduleFromPost(r)
	if err == nil {
		err = playlist.UpdatePlaylist(r.Context(), s.db, playlist.ByID(id), playlist.Update{Name: &name, Schedule: edit, Seen: &seen})
	}
	switch {
	case err == nil:
		http.Redirect(w, r, "/playlists#playlist-"+strconv.FormatInt(id, 10), http.StatusSeeOther)
	case errors.Is(err, playlist.ErrNoPlaylist):
		s.renderNotFound(w, r, noPlaylist)
	case errors.Is(err, playlist.ErrPlaylistChanged):
		form.Changed = true
		s.renderPlaylist(w, r, http.StatusConflict, id, &form, nil, "")
	case errors.Is(err, playlist.ErrNameTaken):
		s.renderPlaylist(w, r, http.StatusConflict, id, &form, nil, nameTakenText())
	case errors.Is(err, errkind.ErrInvalid):
		s.renderPlaylist(w, r, http.StatusBadRequest, id, &form, nil, kindText(err))
	default:
		serverError(w, r, "save playlist", err)
	}
}

// seenFromPost reads the name and schedule the name and schedule form says
// its page showed, from its hidden fields. It returns false if one is missing
// or is not a whole number where it must be.
func seenFromPost(post url.Values) (playlist.Seen, bool) {
	if !post.Has("seen_name") {
		return playlist.Seen{}, false
	}
	seen := playlist.Seen{Name: post.Get("seen_name")}
	for key, field := range map[string]*int64{
		"seen_days": &seen.Schedule.Days, "seen_window_start": &seen.Schedule.WindowStart, "seen_window_end": &seen.Schedule.WindowEnd,
		"seen_starts_at": &seen.Schedule.StartsAt, "seen_ends_at": &seen.Schedule.EndsAt,
	} {
		n, err := strconv.ParseInt(post.Get(key), 10, 64)
		if err != nil {
			return playlist.Seen{}, false
		}
		*field = n
	}
	return seen, true
}

// nameTakenText is the message for a playlist name that another playlist has.
func nameTakenText() string {
	return capitalize(playlist.ErrNameTaken.Error()) + "."
}

// createPlaylist makes an empty playlist with the name the form gives, and
// goes to its page, where items and devices are added.
func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	var form newPlaylistForm
	if !parseForm(w, r, func(status int, msg string) { s.renderPlaylists(w, r, status, "", newPlaylistForm{Error: msg}) }) {
		return
	}
	form.Name = r.PostForm.Get("name")
	pl, err := playlist.CreatePlaylist(r.Context(), s.db, form.Name)
	switch {
	case err == nil:
		http.Redirect(w, r, "/playlists/"+strconv.FormatInt(pl.ID, 10), http.StatusSeeOther)
	case errors.Is(err, playlist.ErrNameTaken):
		form.Error = nameTakenText()
		s.renderPlaylists(w, r, http.StatusConflict, "", form)
	case errors.Is(err, errkind.ErrInvalid):
		form.Error = kindText(err)
		s.renderPlaylists(w, r, http.StatusBadRequest, "", form)
	default:
		serverError(w, r, "create playlist", err)
	}
}

// linkDevice links the device the form names to a playlist, last in the
// device's order.
func (s *Server) linkDevice(w http.ResponseWriter, r *http.Request) {
	s.linkAction(func(ctx context.Context, id int64, dev string) error {
		return playlist.LinkDevice(ctx, s.db, playlist.ByID(id), dev)
	})(w, r)
}

// unlinkDevice unlinks the device the form names from a playlist.
func (s *Server) unlinkDevice(w http.ResponseWriter, r *http.Request) {
	s.linkAction(func(ctx context.Context, id int64, dev string) error {
		err := playlist.UnlinkDevice(ctx, s.db, playlist.ByID(id), dev)
		// A link that is gone already, such as from a page that was out of
		// date, is what the admin asked for, so it is not a failure.
		if errors.Is(err, playlist.ErrNotLinked) {
			return nil
		}
		return err
	})(w, r)
}

// linkAction returns the handler of a form that changes whether the device
// it names, by friendly ID in the device field, is linked to the playlist
// of the path, and then goes back to the devices panel of its page. A
// playlist that does not exist gets the not-found page, and a refused change
// the page with the reason.
func (s *Server) linkAction(do func(ctx context.Context, id int64, dev string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idFromPath(r)
		if !ok {
			s.renderNotFound(w, r, noPlaylist)
			return
		}
		if !parseForm(w, r, func(status int, msg string) { s.renderPlaylist(w, r, status, id, nil, nil, msg) }) {
			return
		}
		dev := strings.TrimSpace(r.PostForm.Get("device"))
		if dev == "" {
			s.renderPlaylist(w, r, http.StatusBadRequest, id, nil, nil, "Choose a device.")
			return
		}
		err := do(r.Context(), id, dev)
		switch {
		case err == nil:
			http.Redirect(w, r, "/playlists/"+strconv.FormatInt(id, 10)+"#devices", http.StatusSeeOther)
		case errors.Is(err, playlist.ErrNoPlaylist):
			s.renderNotFound(w, r, noPlaylist)
		case errors.Is(err, device.ErrNoDevice):
			s.renderPlaylist(w, r, http.StatusBadRequest, id, nil, nil, "That device no longer exists.")
		case errors.Is(err, errkind.ErrInvalid):
			s.renderPlaylist(w, r, http.StatusBadRequest, id, nil, nil, kindText(err))
		default:
			serverError(w, r, "change playlist link", err)
		}
	}
}

// addPlaylistItem appends an item for the instance and duration the add form
// names. playlist.AddItem is what says the playlist is missing, so one that
// was deleted after the page was shown is never made again.
func (s *Server) addPlaylistItem(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, noPlaylist)
		return
	}
	add := &addForm{}
	if !parseForm(w, r, func(status int, msg string) { s.renderPlaylist(w, r, status, id, nil, add, msg) }) {
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
		_, err = playlist.AddItem(r.Context(), s.db, playlist.ByID(id), "", instance, dur)
	}
	switch {
	case err == nil:
		http.Redirect(w, r, "/playlists#playlist-"+strconv.FormatInt(id, 10), http.StatusSeeOther)
	case errors.Is(err, playlist.ErrNoPlaylist):
		s.renderNotFound(w, r, noPlaylist)
	case errors.Is(err, errkind.ErrInvalid):
		s.renderPlaylist(w, r, http.StatusBadRequest, id, nil, add, kindText(err))
	case errors.Is(err, errkind.ErrNotFound):
		// The instance was removed after the page was shown.
		s.renderPlaylist(w, r, http.StatusBadRequest, id, nil, add, "That instance no longer exists.")
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

// itemDeleteView is the page that asks to confirm removing an item.
type itemDeleteView struct {
	// Action is the path the Remove button posts to, and Cancel the page the
	// link goes back to.
	Action, Cancel     string
	Instance, Playlist string
}

// confirmDeleteItem asks whether to remove an item. It changes nothing.
func (s *Server) confirmDeleteItem(w http.ResponseWriter, r *http.Request) {
	row, ok := s.itemFromPath(w, r)
	if !ok {
		return
	}
	item := strconv.FormatInt(row.PlaylistItem.ID, 10)
	d := &itemDeleteView{
		Action:   "/playlists/items/" + item + "/delete",
		Cancel:   "/playlists#playlist-" + strconv.FormatInt(row.PlaylistItem.PlaylistID, 10),
		Instance: row.InstanceName, Playlist: row.PlaylistName,
	}
	s.render(w, r, http.StatusOK, "playlist_item_delete", &pageData{Nav: "playlists", Heading: "Remove item", ItemDelete: d})
}

// playlistDeleteView is the page that asks to confirm deleting a playlist.
type playlistDeleteView struct {
	// Action is the path the Delete button posts to, and Cancel the page the
	// link goes back to.
	Action, Cancel string
	Name           string
	// Summary says what the delete takes with it, from the real counts.
	Summary string
	// Items and Devices are what the summary was made from: the item count
	// and the IDs of the linked devices, comma-separated. The form sends
	// them back so a delete from a page that is out of date is refused.
	Items   int64
	Devices string
}

// confirmDeletePlaylist asks whether to delete a playlist. It changes
// nothing.
func (s *Server) confirmDeletePlaylist(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, noPlaylist)
		return
	}
	s.renderDeleteConfirm(w, r, http.StatusOK, id, "")
}

// renderDeleteConfirm sends the confirm page for the playlist with the ID,
// which gets the not-found page if it does not exist, with msg above the
// summary, or "". The summary and the values the form sends back are one
// snapshot of what goes with the playlist.
func (s *Server) renderDeleteConfirm(w http.ResponseWriter, r *http.Request, status int, id int64, msg string) {
	ctx := r.Context()
	var pl db.Playlist
	var items int64
	var devs []db.ListPlaylistDevicesOfRow
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		if pl, err = q.GetPlaylist(ctx, id); err != nil {
			return fmt.Errorf("get playlist: %w", err)
		}
		if items, err = q.CountPlaylistItems(ctx, id); err != nil {
			return fmt.Errorf("count playlist items: %w", err)
		}
		if devs, err = q.ListPlaylistDevicesOf(ctx, id); err != nil {
			return fmt.Errorf("list playlist devices: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.renderNotFound(w, r, noPlaylist)
		return
	case err != nil:
		serverError(w, r, "read playlist", err)
		return
	}
	names := make([]string, len(devs))
	ids := make([]int64, len(devs))
	for i := range devs {
		names[i] = deviceLabel(devs[i].DeviceName, devs[i].FriendlyID)
		ids[i] = devs[i].DeviceID
	}
	base := strconv.FormatInt(pl.ID, 10)
	v := &playlistDeleteView{
		Action: "/playlists/delete/" + base, Cancel: "/playlists/" + base, Name: pl.Name,
		Summary: deleteSummary(items, names), Items: items, Devices: joinIDs(ids),
	}
	s.render(w, r, status, "playlist_delete", &pageData{Nav: "playlists", Heading: "Delete playlist", Error: msg, PlaylistDelete: v})
}

// deleteSummary says what deleting a playlist with items items and the
// devices called names takes with it. It names up to two devices, and counts
// more.
func deleteSummary(items int64, names []string) string {
	var parts []string
	switch items {
	case 0:
		parts = append(parts, "It has no items.")
	case 1:
		parts = append(parts, "Its 1 item goes with it.")
	default:
		parts = append(parts, fmt.Sprintf("Its %d items go with it.", items))
	}
	switch len(names) {
	case 0:
		parts = append(parts, "No device shows it.")
	case 1:
		parts = append(parts, names[0]+" stops showing it.")
	case 2:
		parts = append(parts, names[0]+" and "+names[1]+" stop showing it.")
	default:
		parts = append(parts, fmt.Sprintf("%d devices stop showing it.", len(names)))
	}
	parts = append(parts, "The instances stay.")
	return strings.Join(parts, " ")
}

// deletePlaylist deletes a playlist with its items, and goes to the list. The
// form sends what the confirm page said goes with it, so a playlist that has
// changed since is not deleted.
func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, noPlaylist)
		return
	}
	if !parseForm(w, r, func(status int, msg string) { s.renderDeleteConfirm(w, r, status, id, msg) }) {
		return
	}
	items, itemsErr := strconv.ParseInt(r.PostForm.Get("items"), 10, 64)
	devices, devicesOK := idList(r.PostForm, "devices")
	if itemsErr != nil || !devicesOK {
		s.renderDeleteConfirm(w, r, http.StatusBadRequest, id, "Reload the page and try again.")
		return
	}
	err := playlist.DeletePlaylist(r.Context(), s.db, playlist.ByID(id), &playlist.Contents{Items: items, Devices: devices})
	switch {
	case err == nil:
		http.Redirect(w, r, "/playlists", http.StatusSeeOther)
	case errors.Is(err, playlist.ErrNoPlaylist):
		s.renderNotFound(w, r, noPlaylist)
	case errors.Is(err, playlist.ErrPlaylistChanged):
		s.renderDeleteConfirm(w, r, http.StatusConflict, id, "This playlist changed after the page loaded, so nothing was deleted. Check what goes with it now.")
	default:
		serverError(w, r, "delete playlist", err)
	}
}
