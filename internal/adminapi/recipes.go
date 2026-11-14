package adminapi

import (
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
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

func (s *Server) getRecipe(_ http.ResponseWriter, r *http.Request) (recipeJSON, error) {
	id, err := pathID(r, recipe.ErrNoRecipe)
	if err != nil {
		return recipeJSON{}, err
	}
	rec, err := s.db.Read.GetRecipe(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return recipeJSON{}, fmt.Errorf("%w: %d", recipe.ErrNoRecipe, id)
	}
	if err != nil {
		return recipeJSON{}, fmt.Errorf("get recipe: %w", err)
	}
	return newRecipe(&rec), nil
}

// installRecipe installs the zip file in the request body.
func (s *Server) installRecipe(w http.ResponseWriter, r *http.Request) (recipeJSON, error) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/zip" {
		return recipeJSON{}, &statusError{status: http.StatusUnsupportedMediaType, msg: "send the recipe as a zip file with Content-Type application/zip"}
	}
	allowSlowUpload(w)
	body := http.MaxBytesReader(w, r.Body, recipe.MaxRecipeBytes)
	rec, err := recipe.InstallZip(r.Context(), s.db, s.cfg.RecipesDir, body, s.now())
	if err != nil {
		return recipeJSON{}, err
	}
	return newRecipe(&rec), nil
}
