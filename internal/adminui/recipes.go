package adminui

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
	"github.com/die-net/trmnl-byos-go/internal/upload"
)

// uploadField is the name of the form field that holds the recipe zip.
const uploadField = "recipe"

// chooseFile is what an upload with no file is told.
const chooseFile = "Choose a recipe zip file to upload."

// devicePalette is what an instance that asks for no palette shows.
const devicePalette = "Device's palette"

var (
	noRecipe   = notFoundView{Nav: "recipes", What: "No such recipe", BackHref: "/recipes", BackName: "Recipes"}
	noInstance = notFoundView{Nav: "recipes", What: "No such instance", BackHref: "/recipes", BackName: "Recipes"}
)

// linkView is a link to another page.
type linkView struct {
	Name, Href string
}

// recipeInstanceView is an instance as the recipe list shows it.
type recipeInstanceView struct {
	ID   int64
	Name string
	// Palette is the name of the palette the instance asks for, or
	// devicePalette for none.
	Palette string
	// Playlists are the playlists that show the instance.
	Playlists []linkView
	// EditHref is the page that edits the instance.
	EditHref string
	// DeleteHref is the page that asks to remove the instance.
	DeleteHref string
}

// recipeView is a recipe as the list shows it.
type recipeView struct {
	ID   int64
	Name string
	// Source is where the recipe came from, in a few words: "Uploaded", or
	// the last element of the path it was installed from. SourceTitle is
	// the full path for a tooltip, or "" for an upload, which has none.
	Source, SourceTitle string
	// Refresh is how often the recipe is rendered, in words.
	Refresh string
	// Installed is the day the recipe was installed.
	Installed timeView
	// Href is the recipe's page, which adds an instance.
	Href      string
	Instances []recipeInstanceView
}

// sourceText returns how the recipe list shows a recipe's stored source,
// and the text for a tooltip. A recorded path can be long, so the list
// shows its last element, and the tooltip the whole path.
func sourceText(source string) (shown, title string) {
	if source == recipe.UploadSource {
		return "Uploaded", ""
	}
	return filepath.Base(source), source
}

// recipeViews describes every recipe with its instances and the playlists
// that show them, from three list queries however many there are, read in
// one snapshot.
func (s *Server) recipeViews(ctx context.Context) ([]recipeView, error) {
	// One snapshot, so every instance's recipe and every playlist link's
	// instance is among the rows read.
	var (
		recipes     []db.Recipe
		insts       []db.ListInstancesRow
		inPlaylists []db.ListInstancePlaylistsRow
	)
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		if recipes, err = q.ListRecipes(ctx); err != nil {
			return fmt.Errorf("list recipes: %w", err)
		}
		if insts, err = q.ListInstances(ctx); err != nil {
			return fmt.Errorf("list instances: %w", err)
		}
		if inPlaylists, err = q.ListInstancePlaylists(ctx); err != nil {
			return fmt.Errorf("list instance playlists: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]recipeView, len(recipes))
	byID := make(map[int64]*recipeView, len(recipes))
	for i := range recipes {
		r := &recipes[i]
		shown, title := sourceText(r.Source)
		out[i] = recipeView{
			ID: r.ID, Name: r.Name, Source: shown, SourceTitle: title, Refresh: "Every " + spanText(r.RefreshInterval),
			Installed: s.dayAt(r.InstalledAt),
			Href:      "/recipes/" + strconv.FormatInt(r.ID, 10),
		}
		byID[r.ID] = &out[i]
	}
	shown := make(map[int64][]linkView)
	for _, p := range inPlaylists {
		shown[p.InstanceID] = append(shown[p.InstanceID], playlistLink(p.PlaylistID, p.PlaylistName))
	}
	for i := range insts {
		inst := &insts[i].PluginInstance
		// The snapshot has the recipe of every instance in it.
		rv := byID[inst.RecipeID]
		v := recipeInstanceView{ID: inst.ID, Name: inst.Name, Palette: devicePalette, Playlists: shown[inst.ID], EditHref: instanceHref(inst.ID), DeleteHref: instanceHref(inst.ID) + "/delete"}
		if inst.Palette != "" {
			// A palette the registry no longer has shows as its ID.
			p, _ := profile.PaletteByID(inst.Palette)
			v.Palette = cmp.Or(p.Name, inst.Palette)
		}
		rv.Instances = append(rv.Instances, v)
	}
	return out, nil
}

// instanceHref is the path of an instance's pages.
func instanceHref(id int64) string {
	return "/recipes/instances/" + strconv.FormatInt(id, 10)
}

// playlistLink is a link to a playlist's card on the playlists page.
func playlistLink(id int64, name string) linkView {
	return linkView{Name: name, Href: "/playlists#playlist-" + strconv.FormatInt(id, 10)}
}

func (s *Server) recipes(w http.ResponseWriter, r *http.Request) {
	s.renderRecipes(w, r, http.StatusOK, "")
}

// renderRecipes sends the recipes page, with msg in the upload panel if it
// is not "".
func (s *Server) renderRecipes(w http.ResponseWriter, r *http.Request, status int, msg string) {
	views, err := s.recipeViews(r.Context())
	if err != nil {
		serverError(w, r, "list recipes", err)
		return
	}
	s.render(w, r, status, "recipes", &pageData{
		Nav: "recipes", Heading: "Recipes", UploadError: msg, Recipes: views,
		UploadLimit: strconv.FormatInt(recipe.MaxRecipeBytes>>20, 10) + " MB",
	})
}

// errNoFile means an upload had no recipe file in it.
var errNoFile = errors.New("no recipe file")

// recipePart returns the part of the multipart body that holds the recipe
// file, or errNoFile, or an error from reading the body: a
// *http.MaxBytesError for a body over its limit, or one wrapping
// upload.ErrCutOff for a body that ended early or is not a form. Any other
// part is skipped without being read.
func recipePart(mr *multipart.Reader) (*multipart.Part, error) {
	for {
		part, err := upload.NextPart(mr)
		switch {
		case err != nil:
			return nil, err
		case part == nil:
			return nil, errNoFile
		}
		if part.FormName() != uploadField {
			continue
		}
		if part.FileName() == "" {
			// A browser sends the field with no name or content when no
			// file was chosen.
			return nil, errNoFile
		}
		return part, nil
	}
}

// uploadRecipe installs the zip file the upload form posts. The body is
// streamed, so the zip is never held in memory: the file part goes straight
// to recipe.InstallZip, which copies it to a temporary file. A body that
// ends early is the client's, not a server failure, so the part is read
// through upload.Reader, and an error from walking the parts goes through
// upload.CutOff.
func (s *Server) uploadRecipe(w http.ResponseWriter, r *http.Request) {
	upload.AllowSlow(w)
	// Room for the recipe and the multipart framing around it, which is a
	// few hundred bytes per field.
	r.Body = http.MaxBytesReader(w, r.Body, recipe.MaxRecipeBytes+maxFormBody)
	mr, err := r.MultipartReader()
	if err != nil {
		s.renderRecipes(w, r, http.StatusBadRequest, chooseFile)
		return
	}
	// Until the file part is found, a body over its limit is the body's:
	// the other parts never end, or are many.
	tooBig := "The upload is too large."
	var rec db.Recipe
	part, err := recipePart(mr)
	if err == nil {
		// Once the file is being read, its own limit is the one that can
		// trip. It is the one the API gives the zip.
		tooBig = fmt.Sprintf("The file is over %d MB.", recipe.MaxRecipeBytes>>20)
		file := upload.Reader(http.MaxBytesReader(w, part, recipe.MaxRecipeBytes))
		rec, err = recipe.InstallZip(r.Context(), s.db, s.recipesDir, file, part.FileName(), s.now())
	}
	var limit *http.MaxBytesError
	switch {
	case err == nil:
		http.Redirect(w, r, "/recipes#recipe-"+strconv.FormatInt(rec.ID, 10), http.StatusSeeOther)
	case errors.As(err, &limit):
		s.renderRecipes(w, r, http.StatusRequestEntityTooLarge, tooBig)
	case errors.Is(err, errNoFile):
		s.renderRecipes(w, r, http.StatusBadRequest, chooseFile)
	case errors.Is(err, upload.ErrCutOff):
		s.renderRecipes(w, r, http.StatusBadRequest, "The upload was cut off or could not be read.")
	case errors.Is(err, errkind.ErrInvalid):
		s.renderRecipes(w, r, http.StatusBadRequest, kindText(err))
	default:
		serverError(w, r, "install recipe", err)
	}
}

// paletteOption is a choice in the palette list.
type paletteOption struct {
	// ID is the value the form posts, "" for the device's palette.
	ID       string
	Label    string
	Selected bool
}

// instanceForm is the name and palette an instance form shows.
type instanceForm struct {
	Name    string
	Palette string
}

// instanceFormView is what the add and edit instance forms share: the name
// and palette, then a control for each custom field of the recipe.
type instanceFormView struct {
	// Action is the path the form posts to.
	Action string
	// NamePlaceholder is the recipe's name, which an empty name means.
	NamePlaceholder string
	Form            instanceForm
	Palettes        []paletteOption
	Fields          []fieldView
	// Unreadable says the instance's stored secrets cannot be read, so the
	// form cannot say which are set.
	Unreadable bool
	// Orphans are the refusals that name no field the form shows.
	Orphans []string
	// Submit is the text of the button.
	Submit string
	// NameNotes and PaletteNotes are, on a refused stale save, the notes
	// under a name or palette that is not the one stored now.
	NameNotes, PaletteNotes []string
	// Seen is the recipe.InstanceState of the instance the edit form shows,
	// which the form sends back so a save from a page that is out of date is
	// refused. It is "" for the add form.
	Seen string
}

// newInstanceForm returns the form view for the fields as src shows them.
// The refusal of the errors in src is a Check the fields line, returned as
// msg unless it is given.
func newInstanceForm(action, namePlaceholder, submit string, form *instanceForm, fields []recipe.Field, src *fieldSource, msg string) (v instanceFormView, top string) {
	pals := profile.Palettes()
	v = instanceFormView{
		Action: action, NamePlaceholder: namePlaceholder, Submit: submit, Form: *form,
		Palettes: make([]paletteOption, 0, len(pals)+1),
		// Nil names are an unknown, which only an edit has.
		Unreadable: src.editing && src.secretNames == nil,
	}
	v.Palettes = append(v.Palettes, paletteOption{Label: devicePalette, Selected: form.Palette == ""})
	for _, p := range pals {
		v.Palettes = append(v.Palettes, paletteOption{ID: p.ID, Label: p.Name, Selected: p.ID == form.Palette})
	}
	var shown int
	v.Fields, v.Orphans, shown = src.build(fields)
	if msg == "" && shown > 0 {
		msg = "Check the fields below."
	}
	return v, msg
}

// recipeEdit is a recipe as its page shows it.
type recipeEdit struct {
	Name string
	// Revision is the recipe's revision the form was drawn from, which the
	// form sends back so an instance is not added from fields of files that
	// were replaced since.
	Revision int64
	instanceFormView
}

// recipeFromPath returns the recipe the path names.
func (s *Server) recipeFromPath(w http.ResponseWriter, r *http.Request) (db.Recipe, bool) {
	return fromPath(s, w, r, "get recipe", noRecipe, s.db.Read.GetRecipe)
}

// recipeFields returns the custom fields of the recipe. It reads a file, so
// it is called once for a request and outside any transaction.
func (s *Server) recipeFields(w http.ResponseWriter, r *http.Request, rec *db.Recipe) ([]recipe.Field, bool) {
	settings, err := recipe.ReadSettings(recipe.FilesDir(s.recipesDir, rec))
	if err != nil {
		serverError(w, r, "read recipe settings", err)
		return nil, false
	}
	return settings.CustomFields, true
}

func (s *Server) recipePage(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.recipeFromPath(w, r)
	if !ok {
		return
	}
	fields, ok := s.recipeFields(w, r, &rec)
	if !ok {
		return
	}
	s.renderRecipe(w, r, http.StatusOK, &rec, rec.Revision, fields, &instanceForm{}, &fieldSource{}, "")
}

// renderRecipe sends the page of a recipe with the form's values, and an
// error message above the form if msg is not "". The form sends revision
// back. src says what the field controls show.
func (s *Server) renderRecipe(w http.ResponseWriter, r *http.Request, status int, rec *db.Recipe, revision int64, fields []recipe.Field, form *instanceForm, src *fieldSource, msg string) {
	v, top := newInstanceForm("/recipes/add-instance/"+strconv.FormatInt(rec.ID, 10), rec.Name, "Add", form, fields, src, msg)
	s.render(w, r, status, "recipe", &pageData{Nav: "recipes", Heading: rec.Name, Error: top, RecipeEdit: &recipeEdit{Name: rec.Name, Revision: revision, instanceFormView: v}})
}

// recipeChanged is what an add from a page drawn from files that were since
// replaced says.
const recipeChanged = "This recipe was replaced after the page loaded, so nothing was added. The form now shows its current fields. Check them and add again."

// addInstance creates an instance of a recipe from the form's name, palette
// and field values. The form sends the recipe's revision it was drawn from,
// so an add from a page that is out of date is refused instead of using
// fields the recipe no longer has. A missing or malformed revision is 0,
// which checks nothing.
func (s *Server) addInstance(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.recipeFromPath(w, r)
	if !ok {
		return
	}
	fields, ok := s.recipeFields(w, r, &rec)
	if !ok {
		return
	}
	form := &instanceForm{}
	if !parseFormLimit(w, r, maxInstanceFormBody, func(status int, msg string) {
		s.renderRecipe(w, r, status, &rec, rec.Revision, fields, form, &fieldSource{}, msg)
	}) {
		return
	}
	form.Name = strings.TrimSpace(r.PostForm.Get("name"))
	form.Palette = r.PostForm.Get("palette")
	revision, _ := strconv.ParseInt(r.PostForm.Get("revision"), 10, 64)
	_, err := recipe.CreateInstance(r.Context(), s.db, s.secrets, s.recipesDir, recipe.InstanceParams{
		RecipeID: rec.ID, Name: form.Name, Palette: form.Palette,
		Values: postedValues(fields, r.PostForm), Secret: markedSecret(fields, r.PostForm), Revision: revision,
	}, s.now())
	if err == nil {
		http.Redirect(w, r, "/recipes#recipe-"+strconv.FormatInt(rec.ID, 10), http.StatusSeeOther)
		return
	}
	src := &fieldSource{posted: r.PostForm}
	if errors.Is(err, recipe.ErrNoRecipe) {
		s.renderNotFound(w, r, noRecipe)
		return
	}
	if errors.Is(err, recipe.ErrRecipeChanged) {
		// The page is drawn again from the recipe as it is now, with what
		// was typed, as an instance page that is out of date is, so the
		// admin checks the fields and adds again.
		if rec, ok = s.recipeFromPath(w, r); !ok {
			return
		}
		if fields, ok = s.recipeFields(w, r, &rec); !ok {
			return
		}
		s.renderRecipe(w, r, http.StatusConflict, &rec, rec.Revision, fields, form, src, recipeChanged)
		return
	}
	status, msg, ok := refusal(err, src)
	if !ok {
		serverError(w, r, "add instance", err)
		return
	}
	s.renderRecipe(w, r, status, &rec, cmp.Or(revision, rec.Revision), fields, form, src, msg)
}

// noKeyText is what a refusal for want of a secrets key says.
const noKeyText = "The server has no secrets key, so a secret value cannot be saved or kept. Set " + secrets.EnvVar + " on the server and restart it."

// refusal says how a page answers an error from CreateInstance or
// UpdateInstance that is the admin's to fix: the status, the message to show
// above the form, and false for any other error. The errors of a field it
// leaves in src, which the form shows in the field's block.
func refusal(err error, src *fieldSource) (status int, msg string, ok bool) {
	var fieldErrs recipe.FieldErrors
	switch {
	case errors.As(err, &fieldErrs):
		src.errs = fieldErrs
		return http.StatusBadRequest, "", true
	case errors.Is(err, secrets.ErrNoKey):
		return http.StatusBadRequest, noKeyText, true
	case errors.Is(err, errkind.ErrInvalid):
		return http.StatusBadRequest, kindText(err), true
	}
	return 0, "", false
}

// instanceDeleteView is the page that asks to confirm removing an instance.
type instanceDeleteView struct {
	// Action is the path the Remove button posts to, and Cancel the page the
	// link goes back to.
	Action, Cancel string
	Name           string
	// Playlists are the playlists that show the instance. While there are
	// any, it cannot be removed.
	Playlists []linkView
}

// instanceFromPath returns the instance the path names.
func (s *Server) instanceFromPath(w http.ResponseWriter, r *http.Request) (db.PluginInstance, bool) {
	return fromPath(s, w, r, "get instance", noInstance, s.db.Read.GetInstance)
}

// renderInstanceDelete sends the confirm page for the instance with the ID,
// which has no Remove button while a playlist shows the instance. The
// instance and its playlists are read in one snapshot, and an instance that
// is gone gets the not-found page.
func (s *Server) renderInstanceDelete(w http.ResponseWriter, r *http.Request, status int, id int64) {
	ctx := r.Context()
	var inst db.PluginInstance
	var rows []db.ListPlaylistsOfInstanceRow
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		if inst, err = q.GetInstance(ctx, id); err != nil {
			return fmt.Errorf("get instance: %w", err)
		}
		if rows, err = q.ListPlaylistsOfInstance(ctx, id); err != nil {
			return fmt.Errorf("list instance playlists: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.renderNotFound(w, r, noInstance)
		return
	case err != nil:
		serverError(w, r, "read instance", err)
		return
	}
	d := &instanceDeleteView{
		Action: instanceHref(inst.ID) + "/delete", Cancel: "/recipes#recipe-" + strconv.FormatInt(inst.RecipeID, 10), Name: inst.Name,
	}
	for _, p := range rows {
		d.Playlists = append(d.Playlists, playlistLink(p.PlaylistID, p.PlaylistName))
	}
	s.render(w, r, status, "instance_delete", &pageData{Nav: "recipes", Heading: "Remove instance", InstanceDelete: d})
}

// confirmRemoveInstance asks whether to remove an instance. It changes
// nothing.
func (s *Server) confirmRemoveInstance(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, noInstance)
		return
	}
	s.renderInstanceDelete(w, r, http.StatusOK, id)
}

// removeInstance removes an instance that no playlist shows.
func (s *Server) removeInstance(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.instanceFromPath(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r, func(status int, _ string) { s.renderInstanceDelete(w, r, status, inst.ID) }) {
		return
	}
	err := recipe.DeleteInstance(r.Context(), s.db, inst.ID)
	switch {
	case err == nil:
		http.Redirect(w, r, "/recipes#recipe-"+strconv.FormatInt(inst.RecipeID, 10), http.StatusSeeOther)
	case errors.Is(err, recipe.ErrNoInstance):
		s.renderNotFound(w, r, noInstance)
	case errors.Is(err, recipe.ErrInstanceInUse):
		// The error's text only counts the items, so the page shows the
		// playlists instead.
		s.renderInstanceDelete(w, r, http.StatusConflict, inst.ID)
	default:
		serverError(w, r, "remove instance", err)
	}
}
