package profile

import (
	"slices"
	"strings"
	"testing"
)

func ptr(v int64) *int64 { return &v }

func TestResolve(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		fwModel       string
		width, height *int64
		model         string
		w, h          int
		limit         int64
		levels        int
		measured      bool
	}{
		{"OG DIY kit", "xiao_epaper_display", ptr(800), ptr(480), "og_plus", 800, 480, 750000, 4, true},
		{"OG DIY kit without size headers", "xiao_epaper_display", nil, nil, "og_plus", 800, 480, 750000, 4, true},
		{"OG", "og", nil, nil, "og_plus", 800, 480, 90000, 4, false},
		{"OG 4-color", "og_4clr", nil, nil, "og_bwry", 800, 480, 90000, 2, false},
		{"TRMNL X", "x", nil, nil, "v2", 1872, 1404, 750000, 16, false},
		{"registry name as Model", "xteink_x4", nil, nil, "xteink_x4", 800, 480, 90000, 2, false},
		{"no limit in the registry", "byod_custom", nil, nil, "byod_custom", 800, 480, 0, 256, false},
		{"unknown, with size", "mystery", ptr(1024), ptr(758), "", 1024, 758, FallbackImageSizeLimit, 2, false},
		{"unknown, no size", "", nil, nil, "", 800, 480, FallbackImageSizeLimit, 2, false},
		{"unknown, absurd size", "mystery", ptr(100000), ptr(1), "", 800, 480, FallbackImageSizeLimit, 2, false},
	} {
		p := Resolve(tc.fwModel, tc.width, tc.height, Override{})
		levels := p.GrayLevels()
		if p.Model != tc.model || p.Width != tc.w || p.Height != tc.h || p.ImageSizeLimit != tc.limit ||
			levels != tc.levels || (p.Light != nil) != tc.measured {
			t.Errorf("%s: %+v with %d levels", tc.name, p, levels)
		}
		if tc.measured && len(p.Light) != levels {
			t.Errorf("%s: %d light values for %d levels", tc.name, len(p.Light), levels)
		}
	}
}

// TestResolveOverride checks an admin's override: the model sets the size
// and palettes, the size limit stays the firmware's unless the model's is
// smaller, the measured light stays only for the same model, and a palette
// override sets the default palette when the model takes it.
func TestResolveOverride(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		fwModel  string
		o        Override
		model    string
		w        int
		limit    int64
		def      string
		measured bool
	}{
		{"4-color OG on old firmware", "og", Override{Model: "og_bwry"}, "og_bwry", 800, 90000, "color-4bwry", false},
		{"DIY kit as an X keeps its firmware's limit", "xiao_epaper_display", Override{Model: "v2"}, "v2", 1872, 750000, "gray-16", false},
		{"X as an OG takes the smaller limit", "x", Override{Model: "og_plus"}, "og_plus", 800, 90000, "bw", false},
		{"no registry limit keeps the firmware's", "og", Override{Model: "byod_custom"}, "byod_custom", 800, 90000, "", false},
		{"unknown firmware gets the fallback limit", "mystery", Override{Model: "v2"}, "v2", 1872, FallbackImageSizeLimit, "gray-16", false},
		{"same model keeps the measured light", "xiao_epaper_display", Override{Model: "og_plus"}, "og_plus", 800, 750000, "bw", true},
		{"palette only", "xiao_epaper_display", Override{Palette: "gray-4"}, "og_plus", 800, 750000, "gray-4", true},
		{"palette the model lacks is ignored", "og", Override{Palette: "gray-16"}, "og_plus", 800, 90000, "bw", false},
		{"unknown model is ignored", "og", Override{Model: "og_nope"}, "og_plus", 800, 90000, "bw", false},
	} {
		p := Resolve(tc.fwModel, nil, nil, tc.o)
		def := p.DefaultPalette
		if tc.def == "" {
			def = ""
		}
		if p.Model != tc.model || p.Width != tc.w || p.ImageSizeLimit != tc.limit || def != tc.def || (p.Light != nil) != tc.measured {
			t.Errorf("%s: %+v", tc.name, p)
		}
	}
}

func TestModels(t *testing.T) {
	t.Parallel()
	models := Models()
	if len(models) < 10 || !slices.IsSortedFunc(models, func(a, b Model) int { return strings.Compare(a.Name, b.Name) }) {
		t.Errorf("Models = %d models, sorted %v", len(models), slices.IsSortedFunc(models, func(a, b Model) int { return strings.Compare(a.Name, b.Name) }))
	}
	if m, ok := ModelByName("og_bwry"); !ok || m.Width != 800 {
		t.Errorf("ModelByName(og_bwry) = %+v, %v", m, ok)
	}
	if _, ok := ModelByName(""); ok {
		t.Error("the empty name is a model")
	}
}

// TestFirmwareModelsInRegistry checks that every firmware Model the server
// maps names a model in the vendored registry, so a registry update that
// renames one is caught.
func TestFirmwareModelsInRegistry(t *testing.T) {
	t.Parallel()
	reg := loadRegistry()
	for fw, m := range firmwareModels {
		if _, ok := reg.models[m.registry]; !ok {
			t.Errorf("firmware model %q maps to %q, which the registry lacks", fw, m.registry)
		}
	}
}

func TestFits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		limit, size int64
		want        bool
	}{
		{90000, 89999, true},
		{90000, 90000, true},
		{90000, 90001, false},
		{0, 1 << 30, true},
	} {
		if got := (Profile{ImageSizeLimit: tc.limit}).Fits(tc.size); got != tc.want {
			t.Errorf("limit %d, size %d: %v, want %v", tc.limit, tc.size, got, tc.want)
		}
	}
}

func TestRenderPalette(t *testing.T) {
	t.Parallel()
	og := Profile{Palettes: []string{"gray-4", "bw"}, DefaultPalette: "gray-4"}
	for _, tc := range []struct{ want, got string }{
		{"", "gray-4"},
		{"bw", "bw"},
		{"gray-16", "gray-4"},
	} {
		if got := og.RenderPalette(tc.want); got != tc.got {
			t.Errorf("RenderPalette(%q) = %q, want %q", tc.want, got, tc.got)
		}
	}
}

func TestPalettes(t *testing.T) {
	t.Parallel()
	pals := Palettes()
	if len(pals) == 0 {
		t.Fatal("no palettes")
	}
	// The registry file starts with the 1-bit palette.
	if pals[0].ID != "bw" || pals[0].Name == "" || pals[0].Grays != 2 {
		t.Errorf("first palette = %+v, want bw with a name and 2 grays", pals[0])
	}
	seen := map[string]bool{}
	for _, p := range pals {
		if seen[p.ID] {
			t.Errorf("palette %q is listed twice", p.ID)
		}
		seen[p.ID] = true
		if p.Name == "" || !KnownPalette(p.ID) {
			t.Errorf("palette %+v has no name or is not known", p)
		}
	}
}

func TestPaletteByID(t *testing.T) {
	t.Parallel()
	for _, p := range Palettes() {
		if got, ok := PaletteByID(p.ID); !ok || got != p {
			t.Errorf("PaletteByID(%q) = %+v, %v; want %+v", p.ID, got, ok, p)
		}
	}
	for _, id := range []string{"", "nope", "BW"} {
		if got, ok := PaletteByID(id); ok || got != (Palette{}) {
			t.Errorf("PaletteByID(%q) = %+v, %v; want none", id, got, ok)
		}
		if KnownPalette(id) {
			t.Errorf("KnownPalette(%q) = true", id)
		}
	}
}
