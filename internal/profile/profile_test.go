package profile

import "testing"

func ptr(v int64) *int64 { return &v }

func TestResolve(t *testing.T) {
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
		p, err := Resolve(tc.fwModel, tc.width, tc.height)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		levels, err := p.GrayLevels()
		if err != nil {
			t.Fatal(err)
		}
		if p.Model != tc.model || p.Width != tc.w || p.Height != tc.h || p.ImageSizeLimit != tc.limit ||
			levels != tc.levels || (p.Light != nil) != tc.measured {
			t.Errorf("%s: %+v with %d levels", tc.name, p, levels)
		}
		if tc.measured && len(p.Light) != levels {
			t.Errorf("%s: %d light values for %d levels", tc.name, len(p.Light), levels)
		}
	}
}

// TestFirmwareModelsInRegistry checks that every firmware Model the server
// maps names a model in the vendored registry, so a registry update that
// renames one is caught.
func TestFirmwareModelsInRegistry(t *testing.T) {
	reg, err := loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for fw, m := range firmwareModels {
		if _, ok := reg.models[m.registry]; !ok {
			t.Errorf("firmware model %q maps to %q, which the registry lacks", fw, m.registry)
		}
	}
}

func TestFits(t *testing.T) {
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
