package adminui

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
)

// instanceEdit is an instance as its edit page shows it.
type instanceEdit struct {
	Name string
	// Back is the recipes list at the instance's recipe.
	Back string
	instanceFormView
}

// instanceState is what the edit page of an instance shows, read once per
// request.
type instanceState struct {
	inst   db.PluginInstance
	rec    db.Recipe
	fields []recipe.Field
	// stored are the plain values, secretNames the keynames of the secret
	// fields that have a value (nil when they cannot be read), and marks the
	// keynames marked secret.
	stored      map[string]recipe.Value
	secretNames []string
	marks       []string
	// state is the recipe.InstanceState of inst, which the form sends back.
	state string
}

// loadInstance reads what the pages of the instance the path names show. The
// instance and its recipe are read in one snapshot, and either one gone gets
// the not-found page. It reads the recipe's settings file, so it is not
// called inside a transaction.
func (s *Server) loadInstance(w http.ResponseWriter, r *http.Request) (*instanceState, bool) {
	id, ok := idFromPath(r)
	if !ok {
		s.renderNotFound(w, r, noInstance)
		return nil, false
	}
	ctx := r.Context()
	st := &instanceState{}
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		if st.inst, err = q.GetInstance(ctx, id); err != nil {
			return fmt.Errorf("get instance: %w", err)
		}
		if st.rec, err = q.GetRecipe(ctx, st.inst.RecipeID); err != nil {
			return fmt.Errorf("get recipe: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.renderNotFound(w, r, noInstance)
		return nil, false
	case err != nil:
		serverError(w, r, "read instance", err)
		return nil, false
	}
	inst := &st.inst
	if st.fields, ok = s.recipeFields(w, r, inst.RecipeID); !ok {
		return nil, false
	}
	if st.stored, st.secretNames, err = recipe.PublicFields(inst, s.secrets); err != nil {
		serverError(w, r, "read instance fields", err)
		return nil, false
	}
	if st.marks, err = recipe.SecretKeys(inst); err != nil {
		serverError(w, r, "read instance marks", err)
		return nil, false
	}
	if st.state, err = recipe.InstanceState(inst); err != nil {
		serverError(w, r, "read instance state", err)
		return nil, false
	}
	return st, true
}

// source returns the fieldSource of the instance's form, for a refused post
// if posted is not nil.
func (st *instanceState) source(posted map[string][]string) *fieldSource {
	return &fieldSource{posted: posted, editing: true, stored: st.stored, secretNames: st.secretNames, marks: st.marks}
}

func (s *Server) instancePage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	s.renderInstance(w, r, http.StatusOK, st, &instanceForm{Name: st.inst.Name, Palette: st.inst.Palette}, st.source(nil), "")
}

// renderInstance sends the edit page of an instance with the form's values,
// and an error message above the form if msg is not "".
func (s *Server) renderInstance(w http.ResponseWriter, r *http.Request, status int, st *instanceState, form *instanceForm, src *fieldSource, msg string) {
	v, top := newInstanceForm(instanceHref(st.inst.ID), st.rec.Name, "Save", form, st.fields, src, msg)
	v.Seen = st.state
	if src.conflict {
		if form.Name != st.inst.Name {
			v.NameNotes = []string{differsText, storedNow(st.inst.Name)}
		}
		if form.Palette != st.inst.Palette {
			label := devicePalette
			if i := slices.IndexFunc(v.Palettes, func(p paletteOption) bool { return p.ID == st.inst.Palette }); st.inst.Palette != "" && i >= 0 {
				label = v.Palettes[i].Label
			}
			v.PaletteNotes = []string{differsText, storedNow(label)}
		}
	}
	s.render(w, r, status, "instance", &pageData{
		Nav: "recipes", Heading: st.inst.Name, Error: top,
		InstanceEdit: &instanceEdit{Name: st.inst.Name, Back: "/recipes#recipe-" + strconv.FormatInt(st.inst.RecipeID, 10), instanceFormView: v},
	})
}

// instanceChanged is what a save from a page that is out of date says. The
// page it is shown on keeps what was typed, so saving again uses it.
const instanceChanged = "This instance changed after the page loaded, so nothing was saved. Fields that differ from what is stored now say so. Save again to use the values below."

// saveInstance saves the name, palette and field values of the form in one
// recipe.UpdateInstance call. The form sends the recipe.InstanceState the
// page showed, so a save from a page that is out of date is refused instead
// of undoing a change the page never showed.
func (s *Server) saveInstance(w http.ResponseWriter, r *http.Request) {
	st, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	inst := &st.inst
	form := &instanceForm{Name: inst.Name, Palette: inst.Palette}
	if !parseFormLimit(w, r, maxInstanceFormBody, func(status int, msg string) { s.renderInstance(w, r, status, st, form, st.source(nil), msg) }) {
		return
	}
	form.Name = strings.TrimSpace(r.PostForm.Get("name"))
	form.Palette = r.PostForm.Get("palette")
	src := st.source(r.PostForm)
	seen := r.PostForm.Get("seen")
	if seen == "" {
		s.renderInstance(w, r, http.StatusBadRequest, st, form, src, "Reload the page and try again.")
		return
	}
	// The update holds only what differs from the values read above, so it
	// is made only when those are what the page showed. UpdateInstance then
	// checks the same state against the row it writes.
	if seen != st.state {
		src.conflict = true
		s.renderInstance(w, r, http.StatusConflict, st, form, src, instanceChanged)
		return
	}
	u := instanceUpdate(st.fields, r.PostForm, st.stored, st.marks, form.Name, form.Palette)
	u.Seen = &seen
	_, err := recipe.UpdateInstance(r.Context(), s.db, s.secrets, s.recipesDir, inst.ID, u, s.now())
	switch {
	case err == nil:
		s.rebuild()
		http.Redirect(w, r, "/recipes#recipe-"+strconv.FormatInt(inst.RecipeID, 10), http.StatusSeeOther)
		return
	case errors.Is(err, recipe.ErrNoInstance):
		s.renderNotFound(w, r, noInstance)
		return
	case errors.Is(err, recipe.ErrInstanceChanged):
		// It changed after it was read above, so the page shows it as it
		// is now.
		if st, ok = s.loadInstance(w, r); !ok {
			return
		}
		src = st.source(r.PostForm)
		src.conflict = true
		s.renderInstance(w, r, http.StatusConflict, st, form, src, instanceChanged)
		return
	}
	status, msg, ok := refusal(err, src)
	if !ok {
		serverError(w, r, "save instance", err)
		return
	}
	s.renderInstance(w, r, status, st, form, src, msg)
}
