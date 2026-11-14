package adminapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/adminauth"
)

// session signs the admin in and returns the Cookie header value.
func (ta *testAPI) session(t *testing.T) string {
	t.Helper()
	if err := adminauth.SetPassword(t.Context(), ta.db, "correct horse battery", ta.now); err != nil {
		t.Fatal(err)
	}
	tok, err := adminauth.Login(t.Context(), ta.db, "correct horse battery", ta.now)
	if err != nil {
		t.Fatal(err)
	}
	return adminauth.CookieName + "=" + tok
}

// TestSessionAuth checks that the session cookie opens the API, and that a
// write that rests on it is refused when another site made it, while the
// same write with a bearer token is not.
func TestSessionAuth(t *testing.T) {
	ta := newTestAPI(t, false)
	cookie := ta.session(t)
	const join = `{"for_seconds": 60}`
	for _, tc := range []struct {
		name    string
		method  string
		path    string
		body    string
		headers []string
		want    int
	}{
		{"cookie reads", "GET", "/api/v1/devices", "", []string{"Authorization", "", "Cookie", cookie}, 200},
		{"cookie writes with no origin", "PUT", "/api/v1/join", join, []string{"Authorization", "", "Cookie", cookie}, 200},
		{"cookie writes from the same origin", "PUT", "/api/v1/join", join, []string{"Authorization", "", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"}, 200},
		{"cookie writes with a matching Origin", "PUT", "/api/v1/join", join, []string{"Authorization", "", "Cookie", cookie, "Origin", "http://example.com"}, 200},
		{"cookie write from another site", "PUT", "/api/v1/join", join, []string{"Authorization", "", "Cookie", cookie, "Sec-Fetch-Site", "cross-site"}, 403},
		{"cookie write with another Origin", "PUT", "/api/v1/join", join, []string{"Authorization", "", "Cookie", cookie, "Origin", "https://evil.example"}, 403},
		{"cookie read from another site", "GET", "/api/v1/devices", "", []string{"Authorization", "", "Cookie", cookie, "Sec-Fetch-Site", "cross-site"}, 200},
		{"bearer writes with no origin", "PUT", "/api/v1/join", join, nil, 200},
		{"bearer writes from another site", "PUT", "/api/v1/join", join, []string{"Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"}, 200},
		{"a bad bearer is not rescued by the cookie", "GET", "/api/v1/devices", "", []string{"Authorization", "Bearer nope", "Cookie", cookie}, 401},
		{"unknown cookie", "GET", "/api/v1/devices", "", []string{"Authorization", "", "Cookie", adminauth.CookieName + "=nope"}, 401},
		{"no credentials", "GET", "/api/v1/devices", "", []string{"Authorization", ""}, 401},
		{"cookie opens the unknown-route 404", "GET", "/api/v1/nope", "", []string{"Authorization", "", "Cookie", cookie}, 404},
		{"cookie opens the webhook route", "GET", "/api/custom_plugins/1", "", []string{"Authorization", "", "Cookie", cookie}, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := ta.do(t, tc.method, tc.path, strings.NewReader(tc.body), tc.headers...)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			if tc.want == 401 && w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", w.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

// TestSessionAuthEnds checks that a cookie stops working after sign-out,
// after the password changes and after it expires.
func TestSessionAuthEnds(t *testing.T) {
	get := func(ta *testAPI, cookie string) int {
		t.Helper()
		return ta.do(t, "GET", "/api/v1/devices", nil, "Authorization", "", "Cookie", cookie).Code
	}
	t.Run("logout", func(t *testing.T) {
		ta := newTestAPI(t, false)
		cookie := ta.session(t)
		if err := adminauth.Logout(t.Context(), ta.db, strings.TrimPrefix(cookie, adminauth.CookieName+"=")); err != nil {
			t.Fatal(err)
		}
		if got := get(ta, cookie); got != http.StatusUnauthorized {
			t.Fatalf("status %d after logout, want 401", got)
		}
	})
	t.Run("password change", func(t *testing.T) {
		ta := newTestAPI(t, false)
		cookie := ta.session(t)
		if err := adminauth.SetPassword(t.Context(), ta.db, "another password!", ta.now); err != nil {
			t.Fatal(err)
		}
		if got := get(ta, cookie); got != http.StatusUnauthorized {
			t.Fatalf("status %d after a password change, want 401", got)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		ta := newTestAPI(t, false)
		cookie := ta.session(t)
		ta.now = ta.now.Add(adminauth.Lifetime)
		if got := get(ta, cookie); got != http.StatusUnauthorized {
			t.Fatalf("status %d after the lifetime, want 401", got)
		}
	})
	t.Run("sliding", func(t *testing.T) {
		ta := newTestAPI(t, false)
		cookie := ta.session(t)
		ta.now = ta.now.Add(adminauth.Lifetime - time.Hour)
		if got := get(ta, cookie); got != http.StatusOK {
			t.Fatalf("status %d just before expiry, want 200", got)
		}
		ta.now = ta.now.Add(adminauth.Lifetime - time.Hour)
		if got := get(ta, cookie); got != http.StatusOK {
			t.Fatalf("status %d after a use pushed the expiry out, want 200", got)
		}
	})
}
