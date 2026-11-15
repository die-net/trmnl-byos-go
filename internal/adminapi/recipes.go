package adminapi

import (
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/upload"
)

// recipeJSON is an installed recipe.
type recipeJSON struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Source         string `json:"source"`
	RefreshSeconds int64  `json:"refresh_seconds"`
	InstalledAt    int64  `json:"installed_at"`
}

func newRecipe(r *db.Recipe) recipeJSON {
	return recipeJSON{ID: r.ID, Name: r.Name, Source: r.Source, RefreshSeconds: r.RefreshInterval, InstalledAt: r.InstalledAt}
}

func (s *Server) listRecipes(_ http.ResponseWriter, r *http.Request) ([]recipeJSON, error) {
	rows, err := s.db.Read.ListRecipes(r.Context())
	if err != nil {
		return nil, fmt.Errorf("list recipes: %w", err)
	}
	out := make([]recipeJSON, 0, len(rows))
	for i := range rows {
		out = append(out, newRecipe(&rows[i]))
	}
	return out, nil
}

// recipeDetailJSON is an installed recipe with its custom field
// definitions, which the list leaves out, since it would read every
// recipe's settings.yml.
type recipeDetailJSON struct {
	recipeJSON
	CustomFields []fieldJSON `json:"custom_fields"`
}

// fieldJSON is the definition of a custom field: what a form needs to ask
// for its value.
type fieldJSON struct {
	Keyname string `json:"keyname"`
	// Label is what a form calls the field.
	Label string `json:"label"`
	// Type is the field_type as the recipe wrote it.
	Type string `json:"type"`
	// Input is what a form shows, and how the value is checked: text,
	// textarea, number, select, boolean, date, time, password, or none for
	// a display-only field that takes no value.
	Input       string `json:"input"`
	Description string `json:"description"`
	HelpText    string `json:"help_text"`
	Placeholder string `json:"placeholder"`
	// Default is what a blank field gets, left out if there is none.
	Default recipe.Value `json:"default,omitzero"`
	// Required is whether an instance needs a value.
	Required bool `json:"required"`
	// Secret is whether the value is always stored encrypted and never shown
	// back, as a password field's is.
	Secret bool `json:"secret"`
	// CanMarkSecret is whether an instance may mark the field secret.
	CanMarkSecret bool `json:"can_mark_secret"`
	// LooksSecret is whether the field's name looks like a credential, so a
	// form ticks its secret box in advance. It is only a guess.
	LooksSecret bool         `json:"looks_secret"`
	Options     []optionJSON `json:"options,omitempty"`
	Multiple    bool         `json:"multiple,omitempty"`
	Min         *float64     `json:"min,omitempty"`
	Max         *float64     `json:"max,omitempty"`
	// MaxLength is the most characters the value may have, left out for no
	// limit.
	MaxLength int `json:"max_length,omitempty"`
	Rows      int `json:"rows,omitempty"`
}

// optionJSON is a choice of a select field.
type optionJSON struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

func newField(f *recipe.Field) fieldJSON {
	out := fieldJSON{
		Keyname: f.Keyname, Label: f.Label, Type: f.Type, Input: f.Input(), Description: f.Description,
		HelpText: f.HelpText, Placeholder: f.Placeholder, Default: f.Default, Required: f.Required(), Secret: f.Secret(),
		CanMarkSecret: f.CanMarkSecret(), LooksSecret: f.LooksSecret(),
		Multiple: f.Multiple, Min: f.Min, Max: f.Max, MaxLength: f.MaxLength, Rows: f.Rows,
	}
	for _, o := range f.Options {
		out.Options = append(out.Options, optionJSON{Label: o.Label, Value: o.Value})
	}
	return out
}

func (s *Server) getRecipe(_ http.ResponseWriter, r *http.Request) (recipeDetailJSON, error) {
	id, err := pathID(r, recipe.ErrNoRecipe)
	if err != nil {
		return recipeDetailJSON{}, err
	}
	rec, err := s.db.Read.GetRecipe(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return recipeDetailJSON{}, fmt.Errorf("%w: %d", recipe.ErrNoRecipe, id)
	}
	if err != nil {
		return recipeDetailJSON{}, fmt.Errorf("get recipe: %w", err)
	}
	settings, err := recipe.ReadSettings(recipe.Dir(s.cfg.RecipesDir, id))
	if err != nil {
		return recipeDetailJSON{}, fmt.Errorf("recipe %d: %w", id, err)
	}
	out := recipeDetailJSON{recipeJSON: newRecipe(&rec), CustomFields: make([]fieldJSON, len(settings.CustomFields))}
	for i := range settings.CustomFields {
		out.CustomFields[i] = newField(&settings.CustomFields[i])
	}
	return out, nil
}

// installRecipe installs the zip file in the request body. A body that is
// cut off is the client's error, not the server's.
func (s *Server) installRecipe(w http.ResponseWriter, r *http.Request) (recipeJSON, error) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/zip" {
		return recipeJSON{}, &statusError{status: http.StatusUnsupportedMediaType, msg: "send the recipe as a zip file with Content-Type application/zip"}
	}
	upload.AllowSlow(w)
	body := upload.Reader(http.MaxBytesReader(w, r.Body, recipe.MaxRecipeBytes))
	// The route has no file name, so a recipe with no name is called
	// "upload".
	rec, err := recipe.InstallZip(r.Context(), s.db, s.cfg.RecipesDir, body, "", s.now())
	if err != nil {
		return recipeJSON{}, err
	}
	return newRecipe(&rec), nil
}
