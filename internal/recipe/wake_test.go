package recipe

import (
	"testing"
	"time"
)

// TestChangesWake checks that each instance or recipe write that makes a
// render due reports the change through the database client, which wakes
// the scheduler whichever front end made it, and that an instance create,
// which is in no playlist yet, a refused write and a replace that changes
// nothing do not.
func TestChangesWake(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	woken := 0
	f.client.OnChange = func() { woken++ }
	var id int64
	for _, tc := range []struct {
		name    string
		do      func() error
		want    int
		refused bool
	}{
		{"create an instance", func() error {
			inst, err := CreateInstance(ctx, f.client, nil, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}}}, time.Unix(1, 0))
			id = inst.ID
			return err
		}, 0, false},
		{"change its settings", func() error {
			_, err := UpdateInstance(ctx, f.client, nil, f.recipesDir, id, InstanceUpdate{Values: map[string]Value{"city": {One: "Bergen"}}}, time.Unix(2, 0))
			return err
		}, 1, false},
		{"a refused change", func() error {
			_, err := UpdateInstance(ctx, f.client, nil, f.recipesDir, id, InstanceUpdate{Values: map[string]Value{"nosuch": {One: "x"}}}, time.Unix(3, 0))
			return err
		}, 0, true},
		{"push data", func() error {
			_, err := PushData(ctx, f.client, PushParams{InstanceID: id, Add: map[string]any{"a": 1}}, time.Unix(4, 0))
			return err
		}, 1, false},
		{"replace the recipe", func() error {
			_, err := replaceWith(t, f, nil, retyped(t, "city", "string", "text"), time.Unix(5, 0))
			return err
		}, 1, false},
		{"replace it with the same files", func() error {
			_, err := replaceWith(t, f, nil, retyped(t, "city", "string", "text"), time.Unix(6, 0))
			return err
		}, 0, false},
	} {
		woken = 0
		if err := tc.do(); (err != nil) != tc.refused {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if woken != tc.want {
			t.Errorf("%s: %d wake-ups, want %d", tc.name, woken, tc.want)
		}
	}
}
