package adminapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
)

// instanceJSON is a plugin instance. It has the values of its plain
// fields, each a string or, for a multi-select, an array of strings, but of
// its secret fields only the names, never the values. Its secret fields are
// the password fields and the fields in SecretKeys, which the admin marked.
// SecretFields is null when the names cannot be read: the server has no
// TRMNL_SECRET_KEY, or the sealed secrets do not open, as after a key
// change. The names are sealed with the values.
type instanceJSON struct {
	ID           int64                   `json:"id"`
	RecipeID     int64                   `json:"recipe_id"`
	RecipeName   string                  `json:"recipe_name"`
	Name         string                  `json:"name"`
	Palette      string                  `json:"palette"`
	Fields       map[string]recipe.Value `json:"fields"`
	SecretFields []string                `json:"secret_fields"`
	// SecretKeys is the keynames the admin marked secret, set or not, and
	// is always an array. It is readable without the secrets key.
	SecretKeys []string `json:"secret_keys"`
	CreatedAt  int64    `json:"created_at"`
}

func (s *Server) newInstance(inst *db.PluginInstance, recipeName string) (instanceJSON, error) {
	fields, names, err := recipe.PublicFields(inst, s.cfg.Key)
	if err != nil {
		return instanceJSON{}, fmt.Errorf("instance %d: %w", inst.ID, err)
	}
	marks, err := recipe.SecretKeys(inst)
	if err != nil {
		return instanceJSON{}, fmt.Errorf("instance %d: %w", inst.ID, err)
	}
	return instanceJSON{
		ID: inst.ID, RecipeID: inst.RecipeID, RecipeName: recipeName, Name: inst.Name, Palette: inst.Palette,
		Fields: fields, SecretFields: names, SecretKeys: marks, CreatedAt: inst.CreatedAt,
	}, nil
}

func (s *Server) listInstances(_ http.ResponseWriter, r *http.Request) ([]instanceJSON, error) {
	rows, err := s.db.Read.ListInstances(r.Context())
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	out := make([]instanceJSON, 0, len(rows))
	for i := range rows {
		inst, err := s.newInstance(&rows[i].PluginInstance, rows[i].RecipeName)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, nil
}

func (s *Server) getInstance(_ http.ResponseWriter, r *http.Request) (instanceJSON, error) {
	id, err := pathID(r, recipe.ErrNoInstance)
	if err != nil {
		return instanceJSON{}, err
	}
	ctx := r.Context()
	// The instance and its recipe's name are read in one snapshot.
	var out instanceJSON
	err = s.db.ReadTx(ctx, func(q *db.Queries) error {
		inst, err := q.GetInstance(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", recipe.ErrNoInstance, id)
		}
		if err != nil {
			return fmt.Errorf("get instance: %w", err)
		}
		out, err = s.instanceWithRecipe(ctx, q, &inst)
		return err
	})
	return out, err
}

// instanceWithRecipe returns an instance with its recipe's name, read
// through q. A recipe that is gone took the instance with it, as after a
// delete that came in just after a write the caller made, so that wraps
// recipe.ErrNoInstance.
func (s *Server) instanceWithRecipe(ctx context.Context, q *db.Queries, inst *db.PluginInstance) (instanceJSON, error) {
	rec, err := q.GetRecipe(ctx, inst.RecipeID)
	if errors.Is(err, sql.ErrNoRows) {
		return instanceJSON{}, fmt.Errorf("%w: %d", recipe.ErrNoInstance, inst.ID)
	}
	if err != nil {
		return instanceJSON{}, fmt.Errorf("get recipe: %w", err)
	}
	return s.newInstance(inst, rec.Name)
}

// createInstanceRequest is the body of an instance create request.
type createInstanceRequest struct {
	RecipeID int64 `json:"recipe_id"`
	// Name defaults to the recipe's.
	Name string `json:"name"`
	// Palette defaults to the device's.
	Palette string `json:"palette"`
	// Fields are custom field values by keyname: a string, or an array of
	// strings for a multi-select. A secret field's value, a password
	// field's or a marked one's, is stored encrypted.
	Fields map[string]recipe.Value `json:"fields"`
	// MarkSecret marks a field secret by keyname when true, so its value
	// is stored encrypted and never shown back. Only a text field can be
	// marked, and a password field always is.
	MarkSecret map[string]bool `json:"mark_secret"`
}

func (s *Server) createInstance(w http.ResponseWriter, r *http.Request) (instanceJSON, error) {
	var req createInstanceRequest
	if err := decode(w, r, &req, false); err != nil {
		return instanceJSON{}, err
	}
	ctx := r.Context()
	inst, err := recipe.CreateInstance(ctx, s.db, s.cfg.Key, s.cfg.RecipesDir, recipe.InstanceParams{
		RecipeID: req.RecipeID,
		Name:     req.Name,
		Palette:  req.Palette,
		Values:   req.Fields,
		Secret:   req.MarkSecret,
	}, s.now())
	if err != nil {
		return instanceJSON{}, err
	}
	return s.instanceWithRecipe(ctx, s.db.Read, &inst)
}

// patchInstanceRequest is the body of an instance update request. A field
// that is left out stays as it is.
type patchInstanceRequest struct {
	// Name is the new name, "" for the recipe's.
	Name *string `json:"name"`
	// Palette is the new palette, "" for the device's.
	Palette *string `json:"palette"`
	// Fields are the custom field values to change by keyname. A key whose
	// value is "" or [] clears a plain field, and a password field's is
	// ignored, so the secret is kept. A key left out is kept.
	Fields map[string]recipe.Value `json:"fields"`
	// Clear is the keynames of the fields to clear, which is how a secret
	// is cleared.
	Clear []string `json:"clear"`
	// MarkSecret marks a field secret by keyname when true, which moves its
	// value into the encrypted fields, and unmarks it when false. A secret
	// is never shown back, so unmarking a field that has a value needs a new
	// value in Fields or the keyname in Clear. A key left out keeps its mark.
	MarkSecret map[string]bool `json:"mark_secret"`
}

func (s *Server) patchInstance(w http.ResponseWriter, r *http.Request) (instanceJSON, error) {
	id, err := pathID(r, recipe.ErrNoInstance)
	if err != nil {
		return instanceJSON{}, err
	}
	var req patchInstanceRequest
	if err := decode(w, r, &req, false); err != nil {
		return instanceJSON{}, err
	}
	ctx := r.Context()
	inst, err := recipe.UpdateInstance(ctx, s.db, s.cfg.Key, s.cfg.RecipesDir, id, recipe.InstanceUpdate{
		Name:    req.Name,
		Palette: req.Palette,
		Values:  req.Fields,
		Clear:   req.Clear,
		Secret:  req.MarkSecret,
	}, s.now())
	if err != nil {
		return instanceJSON{}, err
	}
	s.rebuild()
	return s.instanceWithRecipe(ctx, s.db.Read, &inst)
}

// rebuild asks the scheduler to look for due renders now, through
// Config.Rebuild, after a change that builds read.
func (s *Server) rebuild() {
	if s.cfg.Rebuild != nil {
		s.cfg.Rebuild()
	}
}

func (s *Server) deleteInstance(r *http.Request) error {
	id, err := pathID(r, recipe.ErrNoInstance)
	if err != nil {
		return err
	}
	return recipe.DeleteInstance(r.Context(), s.db, id)
}
