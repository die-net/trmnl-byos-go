package deviceapi

import (
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/screen"
)

// couldNotRenderScreen is the name of the "Could not render" screen the
// test device gets for the instance called name and the failure.
func (ts *testServer) couldNotRenderScreen(t *testing.T, name, failure string) string {
	t.Helper()
	p := profile.Resolve("xiao_epaper_display", nil, nil, profile.Override{})
	img, _, err := ts.srv.screenSized(screenProfile(p), []screen.Line{
		{Text: "Could not render", Size: 44, Bold: true},
		{Text: name, Size: 28},
		{Text: failure, Size: 18},
	})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// TestDisplayFailedOnNewerSettings checks what a device is shown when a
// build failed after its instance's last good render. A failure of settings
// newer than the images' shows "Could not render" with the error, since the
// images no longer show what the instance is set to. A failure of only newer
// data keeps the images, and so does an unreachable renderer, which is not
// counted in failures and says nothing about the settings.
func TestDisplayFailedOnNewerSettings(t *testing.T) {
	t.Parallel()
	const failure = "trmnlp: undefined variable"
	for _, tc := range []struct {
		name string
		// settingsAt and dataAt are what the failed build read, and
		// counted is the failures it adds. The good render read settings
		// at 10 and no data.
		settingsAt, dataAt, counted int64
		failed                      bool
	}{
		{"newer settings", 20, 0, 1, true},
		{"newer settings and data", 20, 30, 1, true},
		{"newer data only", 10, 30, 1, false},
		{"newer settings, renderer unreachable", 20, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			key, first, _ := ts.playlistDevice(t)
			ctx := t.Context()
			img := ts.saveRender(t, first, "")
			p := profile.Resolve("xiao_epaper_display", nil, nil, profile.Override{})
			target := db.SaveRenderFailureParams{
				InstanceID: first, Width: int64(p.Width), Height: int64(p.Height), Palette: p.RenderPalette(""),
				TimeZone: testZone.String(), Locale: "en",
			}
			// The good render read settings at 10.
			if _, err := ts.db.Writer.ExecContext(ctx, `UPDATE render SET settings_at = 10, rendered_settings_at = 10`); err != nil {
				t.Fatal(err)
			}
			fail := target
			fail.AttemptedAt, fail.Error = ts.now.Unix(), failure
			fail.SettingsAt, fail.DataAt, fail.Failures = tc.settingsAt, tc.dataAt, tc.counted
			if err := ts.db.Write.SaveRenderFailure(ctx, fail); err != nil {
				t.Fatal(err)
			}
			timer := displayHeaders(testMAC, key)
			timer["Update-Source"] = "timer"
			m := ts.getJSON(t, "/api/display", timer)
			want := img
			if tc.failed {
				want = ts.couldNotRenderScreen(t, "Weather", failure)
			}
			if m["filename"] != want {
				t.Errorf("%v; want %s (failed screen: %v)", m, want, tc.failed)
			}
			if tc.failed && m["refresh_rate"] != float64(retryRefresh) {
				t.Errorf("refresh %v, want the retry interval %d", m["refresh_rate"], retryRefresh)
			}
		})
	}
}

// TestDisplayFailedOnNewerSettingsRecovers checks that a good build after
// a failure of newer settings shows its images again, since it read those
// settings.
func TestDisplayFailedOnNewerSettingsRecovers(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key, first, _ := ts.playlistDevice(t)
	ctx := t.Context()
	ts.saveRender(t, first, "")
	p := profile.Resolve("xiao_epaper_display", nil, nil, profile.Override{})
	err := ts.db.Write.SaveRenderFailure(ctx, db.SaveRenderFailureParams{
		InstanceID: first, Width: int64(p.Width), Height: int64(p.Height), Palette: p.RenderPalette(""),
		TimeZone: testZone.String(), Locale: "en", AttemptedAt: ts.now.Unix(), Error: "boom", SettingsAt: 20, Failures: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	timer := displayHeaders(testMAC, key)
	timer["Update-Source"] = "timer"
	if m := ts.getJSON(t, "/api/display", timer); m["filename"] != ts.couldNotRenderScreen(t, "Weather", "boom") {
		t.Fatalf("after the failure: %v; want the Could not render screen", m)
	}
	img := ts.saveRender(t, first, "")
	if _, err := ts.db.Writer.ExecContext(ctx, `UPDATE render SET settings_at = 20, rendered_settings_at = 20`); err != nil {
		t.Fatal(err)
	}
	if m := ts.getJSON(t, "/api/display", timer); m["filename"] != img {
		t.Errorf("after a good build: %v; want its image %s", m, img)
	}
}
