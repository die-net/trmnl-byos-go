package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
)

// replaceHarnessRecipe replaces the harness's recipe with one whose
// full.liquid is markup and whose settings.yml is settings.
func (h *harness) replaceHarnessRecipe(t *testing.T, settings, markup string) recipe.Replaced {
	t.Helper()
	src := t.TempDir()
	for name, body := range map[string]string{"settings.yml": settings, "full.liquid": markup} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := recipe.Replace(t.Context(), h.db, h.s.cfg.Secrets, h.s.cfg.RecipesDir, 1, src, h.now)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestReplaceRebuilds checks that replacing a recipe makes its renders due
// at once, built from the new files, and that a recipe that starts reading
// trmnl.device is then built per device, though the scheduler had scanned
// the old files as shared.
func TestReplaceRebuilds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.renderDue(t, 1)
	if got := h.screenBody(t, 1, 1); strings.Contains(got, "@") {
		t.Fatalf("before the replace: %q, want a shared render", got)
	}
	h.now = h.now.Add(time.Minute)
	h.renderDue(t, 1) // Nothing is due yet.

	h.replaceHarnessRecipe(t, "name: Hourly\nrefresh_interval: 60\ncustom_fields:\n- keyname: token\n  field_type: password\n", "<p>{{ trmnl.device.friendly_id }}</p>")
	h.renderDue(t, 2)
	if got := h.screenBody(t, 1, 1); !strings.HasPrefix(got, "full@ABC123 ") {
		t.Errorf("after the replace: %q, want the device's own render", got)
	}
	h.renderDue(t, 2)
}

// TestBuildGetsDeclaredFieldsOnly checks that a stored value of a field
// the recipe no longer declares is kept but not sent to a build.
func TestBuildGetsDeclaredFieldsOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.renderDue(t, 1)
	inst, err := h.db.Read.GetInstance(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	// A value left from a field a replace dropped.
	_, err = h.db.Write.UpdateInstance(t.Context(), db.UpdateInstanceParams{
		ID: 1, Name: inst.Name, Palette: inst.Palette, FieldValues: `{"city":"Oslo"}`, SecretValues: inst.SecretValues,
		SecretKeys: inst.SecretKeys, SettingsAt: inst.SettingsAt + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.renderDue(t, 2)
	h.mu.Lock()
	last := h.configs[len(h.configs)-1]
	h.mu.Unlock()
	if strings.Contains(last, "city") || !strings.Contains(last, "token") {
		t.Errorf("build's .trmnlp.yml:\n%s\nwant token and no city", last)
	}
}
