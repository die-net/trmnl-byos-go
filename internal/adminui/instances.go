package adminui

import (
	"errors"
	"net/http"
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

// loadInstance reads what the instance's pages show. It reads the recipe's
// settings file, so it is not called inside a transaction.
func (s *Server) loadInstance(w http.ResponseWriter, r *http.Request, inst *db.PluginInstance) (*instanceState, bool) {
	st := &instanceState{inst: *inst}
	var err error
	if st.rec, err = s.db.Read.GetRecipe(r.Context(), inst.RecipeID); err != nil {
		serverError(w, r, "get recipe", err)
		return nil, false
	}
	var ok bool
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
	inst, ok := s.instanceFromPath(w, r)
	if !ok {
		return
	}
	st, ok := s.loadInstance(w, r, &inst)
	if !ok {
		return
	}
	s.renderInstance(w, r, http.StatusOK, st, &instanceForm{Name: inst.Name, Palette: inst.Palette}, st.source(nil), "")
}

// renderInstance sends the edit page of an instance with the form's values,
// and an error message above the form if msg is not "".
func (s *Server) renderInstance(w http.ResponseWriter, r *http.Request, status int, st *instanceState, form *instanceForm, src *fieldSource, msg string) {
	v, top := newInstanceForm(instanceHref(st.inst.ID), st.rec.Name, "Save", form, st.fields, src, msg)
	v.Seen = st.state
	if src.conflict {
		v.NameDiffers, v.PaletteDiffers, v.Differs = form.Name != st.inst.Name, form.Palette != st.inst.Palette, differsText
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
	inst, ok := s.instanceFromPath(w, r)
	if !ok {
		return
	}
	st, ok := s.loadInstance(w, r, &inst)
	if !ok {
		return
	}
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
		http.Redirect(w, r, "/recipes#recipe-"+strconv.FormatInt(inst.RecipeID, 10), http.StatusSeeOther)
		return
	case errors.Is(err, recipe.ErrNoInstance):
		s.renderNotFound(w, r, noInstance)
		return
	case errors.Is(err, recipe.ErrInstanceChanged):
		// It changed after it was read above, so the page shows it as it
		// is now.
		if inst, ok = s.instanceFromPath(w, r); !ok {
			return
		}
		if st, ok = s.loadInstance(w, r, &inst); !ok {
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
