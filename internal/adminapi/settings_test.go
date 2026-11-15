package adminapi

import (
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

func TestSettings(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	var got settingsJSON
	ta.call(t, "GET", "/api/v1/settings", "", 200, &got)
	if got != (settingsJSON{RefreshSeconds: 900, MinPasswordLength: 12}) {
		t.Fatalf("defaults = %+v", got)
	}
	// The fields are numbers, as the API documents.
	if body := ta.call(t, "GET", "/api/v1/settings", "", 200, nil).Body.String(); !strings.Contains(body, `"refresh_seconds":900`) || !strings.Contains(body, `"min_password_length":12`) {
		t.Errorf("body %q", body)
	}

	// A field left out is kept.
	ta.call(t, "PATCH", "/api/v1/settings", `{"refresh_seconds": 600}`, 200, &got)
	if got != (settingsJSON{RefreshSeconds: 600, MinPasswordLength: 12}) {
		t.Fatalf("after one field = %+v", got)
	}
	ta.call(t, "PATCH", "/api/v1/settings", `{"min_password_length": 8}`, 200, &got)
	if got != (settingsJSON{RefreshSeconds: 600, MinPasswordLength: 8}) {
		t.Fatalf("after the other = %+v", got)
	}
	ta.call(t, "PATCH", "/api/v1/settings", `{}`, 200, &got)
	if got != (settingsJSON{RefreshSeconds: 600, MinPasswordLength: 8}) {
		t.Fatalf("after an empty patch = %+v", got)
	}
	ta.call(t, "PATCH", "/api/v1/settings", `{"refresh_seconds": 1, "min_password_length": 1024}`, 200, &got)
	if got != (settingsJSON{RefreshSeconds: 1, MinPasswordLength: 1024}) {
		t.Fatalf("after both = %+v", got)
	}
	var again settingsJSON
	ta.call(t, "GET", "/api/v1/settings", "", 200, &again)
	if again != got {
		t.Errorf("read back %+v, want %+v", again, got)
	}
}

func TestSettingsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, contains string }{
		{"refresh zero", `{"refresh_seconds": 0}`, "refresh_seconds"},
		{"refresh over a year", `{"refresh_seconds": 31536001}`, "refresh_seconds"},
		{"min length zero", `{"min_password_length": 0}`, "min_password_length"},
		{"min length over the maximum", `{"min_password_length": 1025}`, "min_password_length"},
		{"unknown field", `{"refresh_seconds": 60, "join_until": 5}`, "unknown field"},
		{"password hash", `{"admin_password_hash": "x"}`, "unknown field"},
		{"string", `{"refresh_seconds": "60"}`, "bad request body"},
		{"fraction", `{"refresh_seconds": 1.5}`, "bad request body"},
		{"empty body", ``, "bad request body"},
		// The first field is fine and the second is not, so nothing is written.
		{"one bad field", `{"refresh_seconds": 60, "min_password_length": 0}`, "min_password_length"},
		{"one bad field, other order", `{"refresh_seconds": 0, "min_password_length": 20}`, "refresh_seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ta := newTestAPI(t, false)
			ta.wantError(t, "PATCH", "/api/v1/settings", tc.body, 400, tc.contains)
			rows, err := settings.List(t.Context(), ta.db.Read)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.Set {
					t.Errorf("%s was written", r.Key)
				}
			}
		})
	}
}

func TestSettingsBadRow(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	if body := ta.call(t, "GET", "/api/v1/settings", "", 200, nil).Body.String(); strings.Contains(body, "bad") {
		t.Errorf("body %q has bad with no bad row", body)
	}
	ta.call(t, "PATCH", "/api/v1/settings", `{"refresh_seconds": 60}`, 200, nil)
	if err := ta.db.Write.SetSetting(t.Context(), db.SetSettingParams{Key: settings.KeyMinPasswordLength, Value: "", UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	// The number is the default in effect, and bad has the stored text,
	// which here is empty.
	body := ta.call(t, "GET", "/api/v1/settings", "", 200, nil).Body.String()
	if want := `{"refresh_seconds":60,"min_password_length":12,"bad":{"min_password_length":""}}`; strings.TrimSpace(body) != want {
		t.Errorf("body %q, want %q", body, want)
	}
	if err := ta.db.Write.SetSetting(t.Context(), db.SetSettingParams{Key: settings.KeyRefreshSeconds, Value: "abc", UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	body = ta.call(t, "GET", "/api/v1/settings", "", 200, nil).Body.String()
	if want := `{"refresh_seconds":900,"min_password_length":12,"bad":{"refresh_seconds":"abc","min_password_length":""}}`; strings.TrimSpace(body) != want {
		t.Errorf("body %q, want %q", body, want)
	}
	// A patch repairs one, and the answer shows the other still bad.
	var got settingsJSON
	ta.call(t, "PATCH", "/api/v1/settings", `{"min_password_length": 20}`, 200, &got)
	if got.MinPasswordLength != 20 || got.Bad == nil || got.Bad.MinPasswordLength != nil || got.Bad.RefreshSeconds == nil || *got.Bad.RefreshSeconds != "abc" {
		t.Errorf("after repairing one = %+v", got)
	}
	var fixed settingsJSON
	ta.call(t, "PATCH", "/api/v1/settings", `{"refresh_seconds": 120}`, 200, &fixed)
	if fixed != (settingsJSON{RefreshSeconds: 120, MinPasswordLength: 20}) {
		t.Errorf("after repairing both = %+v", fixed)
	}
}

func TestSettingsNeedAuth(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	for _, method := range []string{"GET", "PATCH"} {
		w := ta.do(t, method, "/api/v1/settings", strings.NewReader(`{}`), "Authorization", "Bearer wrong")
		if w.Code != 401 {
			t.Errorf("%s with a bad token: %d", method, w.Code)
		}
	}
}
