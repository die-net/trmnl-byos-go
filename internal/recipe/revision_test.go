package recipe

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// staleRecipe is a db.DBTX whose GetRecipe answers the revision before the
// recipe's own while stale is above 0, once per count, and passes every
// other query to the database. A read through it before a transaction is
// what a read is when a replace commits right after it.
type staleRecipe struct {
	db.DBTX
	stale *atomic.Int32
}

// QueryRowContext answers GetRecipe with the revision one lower while
// stale is above 0.
func (s staleRecipe) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.HasPrefix(query, "-- name: GetRecipe ") && s.stale.Add(-1) >= 0 {
		query = strings.Replace(query, "installed_at, revision,", "installed_at, revision - 1 AS revision,", 1)
	}
	return s.DBTX.QueryRowContext(ctx, query, args...)
}

// TestCheckRevision checks the revision check of an instance write: a
// recipe at the revision the fields were read for passes, one at another
// means the call is made again, and one at a revision other than the
// caller's own form is a conflict, whichever the fields were read for.
func TestCheckRevision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		current, read, seen int64
		want                error
	}{
		{"as read", 2, 2, 0, nil},
		{"as read and seen", 2, 2, 2, nil},
		{"replaced since the read", 3, 2, 0, errRecipeMoved},
		{"replaced since the form", 3, 3, 2, ErrRecipeChanged},
		{"replaced since both", 3, 2, 2, ErrRecipeChanged},
	} {
		err := checkRevision(&db.Recipe{ID: 1, Revision: tc.current}, tc.read, tc.seen)
		if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestInstanceWriteRacesReplace checks a create and an update whose fields
// were read before a replace committed: each is made again with the new
// fields, so a value only the old fields take is refused, and one only the
// new fields take is stored.
func TestInstanceWriteRacesReplace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	inst, err := CreateInstance(t.Context(), f.client, nil, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}}}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Revision 2 drops the imperial units and adds a zip field.
	yml := strings.NewReplacer(
		"options: [Metric, Imperial]", "options: [Metric]",
		"- keyname: days\n", "- keyname: zip\n  field_type: string\n  optional: true\n- keyname: days\n",
	).Replace(weatherYML)
	if _, err := replaceWith(t, f, nil, yml, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	var stale atomic.Int32
	f.client.Read = db.New(staleRecipe{DBTX: f.client.Reader, stale: &stale})
	imperial := map[string]Value{"units": {One: "imperial"}}

	stale.Store(1)
	_, err = UpdateInstance(t.Context(), f.client, nil, f.recipesDir, inst.ID, InstanceUpdate{Values: imperial}, time.Unix(2000, 0))
	var bad FieldErrors
	if !errors.As(err, &bad) || bad["units"] == "" || stale.Load() > 0 {
		t.Errorf("update with a value only the old fields take: %v; want it checked again and refused", err)
	}
	stale.Store(1)
	if _, err := UpdateInstance(t.Context(), f.client, nil, f.recipesDir, inst.ID, InstanceUpdate{Values: map[string]Value{"zip": {One: "5003"}}}, time.Unix(2000, 0)); err != nil {
		t.Errorf("update with a value only the new fields take: %v", err)
	}

	stale.Store(1)
	_, err = CreateInstance(t.Context(), f.client, nil, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Bergen"}, "units": {One: "imperial"}}}, time.Unix(2000, 0))
	if !errors.As(err, &bad) || bad["units"] == "" || stale.Load() > 0 {
		t.Errorf("create with a value only the old fields take: %v; want it checked again and refused", err)
	}

	// Two replaces in a row during one call are a conflict.
	stale.Store(2)
	_, err = CreateInstance(t.Context(), f.client, nil, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Bergen"}}}, time.Unix(2000, 0))
	if !errors.Is(err, ErrRecipeChanged) || !errors.Is(err, errkind.ErrConflict) {
		t.Errorf("create racing two replaces: %v, want ErrRecipeChanged", err)
	}
}
