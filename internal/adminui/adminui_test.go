package adminui

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/die-net/trmnl-byos-go/internal/adminauth"
	"github.com/die-net/trmnl-byos-go/internal/db"
)

const testPassword = "correct horse battery"

// cheapHash returns password as an argon2id PHC string at 64 KiB and one
// pass. adminauth.SetPassword always hashes at the production cost, which
// would make each test spend most of its time hashing. A login checks a
// password at the cost the stored hash names, so this keeps logins cheap too.
func cheapHash(password string) string {
	salt := []byte("test salt, fixed")
	key := argon2.IDKey([]byte(password), salt, 1, 64, 1, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=64,t=1,p=1$%s$%s", argon2.Version, enc.EncodeToString(salt), enc.EncodeToString(key))
}

// testZone is the zone the tests show times in, which no machine runs in.
var testZone = time.FixedZone("TST", 5*3600+1800)

// testUI is a server on a temp database with the admin password set.
type testUI struct {
	srv *Server
	db  *db.Client
	mux *http.ServeMux
	now time.Time
}

func newTestUI(t *testing.T, cfg Config, setPassword bool) *testUI {
	t.Helper()
	client, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	tu := &testUI{db: client, mux: http.NewServeMux(), now: time.Unix(1_000_000, 0)}
	cfg.Zone = testZone
	tu.srv = New(client, cfg)
	// In UTC, as time.Now gives the time in time.Local, so times are shown
	// and schedules read in testZone only if the server uses Config.Zone.
	tu.srv.now = func() time.Time { return tu.now.UTC() }
	tu.srv.Register(tu.mux)
	if setPassword {
		// The setting adminauth keeps the hash in.
		err := client.Write.SetSetting(t.Context(), db.SetSettingParams{Key: "admin_password_hash", Value: cheapHash(testPassword), UpdatedAt: tu.now.Unix()})
		if err != nil {
			t.Fatal(err)
		}
	}
	return tu
}

// do sends a request. headers are name and value pairs.
func (tu *testUI) do(t *testing.T, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return tu.serve(req)
}

func (tu *testUI) serve(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	tu.mux.ServeHTTP(w, req)
	return w
}

// login posts the password and returns the response.
func (tu *testUI) login(t *testing.T, password string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	return tu.do(t, "POST", "/login", url.Values{"password": {password}}.Encode(), headers...)
}

// signIn logs in and returns the session cookie.
func (tu *testUI) signIn(t *testing.T) *http.Cookie {
	t.Helper()
	w := tu.login(t, testPassword)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login status %d: %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == adminauth.CookieName {
			return c
		}
	}
	t.Fatal("login set no session cookie")
	return nil
}

func cookieHeader(c *http.Cookie) string { return c.Name + "=" + c.Value }

func TestLoginCookie(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cfg    Config
		tls    bool
		secure bool
	}{
		{"plain http", Config{}, false, false},
		{"http public URL", Config{PublicURL: "http://192.0.2.1:8080"}, false, false},
		{"tls request", Config{}, true, true},
		{"https public URL", Config{PublicURL: "https://trmnl.example.com"}, false, true},
		{"https public URL, any case", Config{PublicURL: "HTTPS://trmnl.example.com"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, tc.cfg, true)
			req := httptest.NewRequestWithContext(t.Context(), "POST", "/login", strings.NewReader(url.Values{"password": {testPassword}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			w := tu.serve(req)
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
				t.Fatalf("status %d to %q, want 303 to /", w.Code, w.Header().Get("Location"))
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("%d cookies, want 1", len(cookies))
			}
			c := cookies[0]
			if c.Name != "trmnl_session" || len(c.Value) != 43 || c.Path != "/" || !c.HttpOnly ||
				c.SameSite != http.SameSiteLaxMode || c.MaxAge != 30*24*3600 || c.Secure != tc.secure {
				t.Errorf("cookie %+v, want the session cookie with Secure=%v", c, tc.secure)
			}
		})
	}
}

func TestLoginFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		setPassword bool
		password    string
		want        int
		message     string
		notMessage  string
	}{
		{"wrong password", true, "not the password", 401, "Wrong password.", "trmnl-byos password"},
		{"empty password", true, "", 401, "Wrong password.", "trmnl-byos password"},
		{"no password set", false, testPassword, 401, "Run trmnl-byos password", "Wrong password."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, tc.setPassword)
			w := tu.login(t, tc.password)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d", w.Code, tc.want)
			}
			if len(w.Result().Cookies()) != 0 {
				t.Error("a failed login set a cookie")
			}
			body := w.Body.String()
			if !strings.Contains(body, tc.message) || strings.Contains(body, tc.notMessage) {
				t.Errorf("body %q, want it to hold %q and not %q", body, tc.message, tc.notMessage)
			}
		})
	}
}

func TestLoginRedirectIgnoresNext(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	w := tu.do(t, "POST", "/login?next=https://evil.example/", url.Values{"password": {testPassword}, "next": {"https://evil.example/"}}.Encode())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("status %d to %q, want 303 to /", w.Code, w.Header().Get("Location"))
	}
}

func TestPagesNeedSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cookie string
	}{
		{"no cookie", ""},
		{"unknown token", adminauth.CookieName + "=nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			var h []string
			if tc.cookie != "" {
				h = []string{"Cookie", tc.cookie}
			}
			for _, path := range []string{"/", "/devices", "/recipes", "/playlists", "/firmware", "/settings"} {
				w := tu.do(t, "GET", path, "", h...)
				if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
					t.Fatalf("GET %s: status %d to %q, want 303 to /login", path, w.Code, w.Header().Get("Location"))
				}
			}
		})
	}

	t.Run("signed in", func(t *testing.T) {
		t.Parallel()
		tu := newTestUI(t, Config{}, true)
		c := tu.signIn(t)
		if w := tu.do(t, "GET", "/", "", "Cookie", cookieHeader(c)); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/devices" {
			t.Fatalf("signed-in home: status %d to %q, want 303 to /devices", w.Code, w.Header().Get("Location"))
		}
		if w := tu.do(t, "GET", "/devices", "", "Cookie", cookieHeader(c)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `action="/logout"`) {
			t.Fatalf("signed-in devices: status %d, body %q", w.Code, w.Body)
		}
		// The login page sends a signed-in admin home.
		if w := tu.do(t, "GET", "/login", "", "Cookie", cookieHeader(c)); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
			t.Errorf("GET /login signed in: status %d to %q", w.Code, w.Header().Get("Location"))
		}
		// A session that has expired is not one.
		tu.now = tu.now.Add(adminauth.Lifetime)
		if w := tu.do(t, "GET", "/devices", "", "Cookie", cookieHeader(c)); w.Code != http.StatusSeeOther {
			t.Errorf("expired session: status %d, want 303", w.Code)
		}
	})
}

func TestLogout(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{PublicURL: "https://trmnl.example.com"}, true)
	c := tu.signIn(t)
	other := tu.signIn(t)
	w := tu.do(t, "POST", "/logout", "", "Cookie", cookieHeader(c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("status %d to %q, want 303 to /login", w.Code, w.Header().Get("Location"))
	}
	cleared := w.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != adminauth.CookieName || cleared[0].MaxAge >= 0 || !cleared[0].Secure {
		t.Errorf("cookies after logout %+v, want the session cookie cleared", cleared)
	}
	if w := tu.do(t, "GET", "/devices", "", "Cookie", cookieHeader(c)); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("signed-out cookie still opens /devices: status %d", w.Code)
	}
	if w := tu.do(t, "GET", "/devices", "", "Cookie", cookieHeader(other)); w.Code != http.StatusOK {
		t.Errorf("another session was signed out: status %d", w.Code)
	}
}

// TestCrossOrigin checks that a post from another site is refused on login
// and logout, and does not sign anyone out, while a same-origin post and a
// post with neither header, as from a script, go through.
func TestCrossOrigin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		headers []string
		allowed bool
	}{
		{"no origin headers", nil, true},
		{"same-origin fetch metadata", []string{"Sec-Fetch-Site", "same-origin"}, true},
		{"matching Origin", []string{"Origin", "http://example.com"}, true},
		{"cross-site fetch metadata", []string{"Sec-Fetch-Site", "cross-site"}, false},
		{"same-site fetch metadata", []string{"Sec-Fetch-Site", "same-site"}, false},
		{"other Origin", []string{"Origin", "https://evil.example"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			c := tu.signIn(t)
			w := tu.login(t, testPassword, tc.headers...)
			if got := w.Code != http.StatusForbidden; got != tc.allowed {
				t.Errorf("login: status %d, allowed %v, want %v", w.Code, got, tc.allowed)
			}
			if !tc.allowed && len(w.Result().Cookies()) != 0 {
				t.Error("a refused login set a cookie")
			}
			h := append([]string{"Cookie", cookieHeader(c)}, tc.headers...)
			w = tu.do(t, "POST", "/logout", "", h...)
			if got := w.Code != http.StatusForbidden; got != tc.allowed {
				t.Errorf("logout: status %d, allowed %v, want %v", w.Code, got, tc.allowed)
			}
			still := tu.do(t, "GET", "/devices", "", "Cookie", cookieHeader(c)).Code == http.StatusOK
			if still == tc.allowed {
				t.Errorf("session still valid = %v after logout allowed = %v", still, tc.allowed)
			}
		})
	}
}

func TestFormTooBig(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	w := tu.login(t, strings.Repeat("a", maxFormBody+1))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", w.Code)
	}
}

func TestHeaders(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	c := tu.signIn(t)
	for _, tc := range []struct {
		name    string
		method  string
		path    string
		headers []string
		status  int
		noStore bool
	}{
		{"login page", "GET", "/login", nil, 200, true},
		{"failed login", "POST", "/login", nil, 401, true},
		{"devices", "GET", "/devices", []string{"Cookie", cookieHeader(c)}, 200, true},
		{"recipes page", "GET", "/recipes", []string{"Cookie", cookieHeader(c)}, 200, true},
		{"home", "GET", "/", []string{"Cookie", cookieHeader(c)}, 303, true},
		{"home redirect", "GET", "/", nil, 303, true},
		{"refused cross-origin post", "POST", "/logout", []string{"Sec-Fetch-Site", "cross-site"}, 403, true},
		{"css", "GET", "/static/app.css", nil, 200, false},
		{"missing static file", "GET", "/static/nope.css", nil, 404, false},
		{"static directory", "GET", "/static/", nil, 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := ""
			if tc.method == "POST" {
				body = "password=x"
			}
			w := tu.do(t, tc.method, tc.path, body, tc.headers...)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
			h := w.Header()
			if got := h.Get("Content-Security-Policy"); got != "default-src 'self'; frame-ancestors 'none'" {
				t.Errorf("Content-Security-Policy = %q", got)
			}
			if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
			if got := h.Get("Cache-Control") == "no-store"; got != tc.noStore {
				t.Errorf("Cache-Control = %q, no-store wanted %v", h.Get("Cache-Control"), tc.noStore)
			}
		})
	}
}

func TestStaticAndMarkup(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	w := tu.do(t, "GET", "/static/app.css", "")
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("css Content-Type = %q", ct)
	}
	css := w.Body.String()
	for _, want := range []string{`:root[data-theme="dark"]`, `:root[data-theme="light"]`, "prefers-color-scheme: dark", "--accent: #3f6b2a", "--accent: #9ccb7c"} {
		if !strings.Contains(css, want) {
			t.Errorf("css lacks %q", want)
		}
	}

	page := tu.do(t, "GET", "/login", "").Body.String()
	for _, want := range []string{
		`<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">`,
		`<meta name="color-scheme" content="light dark">`,
		`autocomplete="current-password"`,
		`<label for="password">`,
		`href="/static/app.css"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("login page lacks %q", want)
		}
	}
	for _, bad := range []string{"<script", "style=", "<style"} {
		if strings.Contains(page, bad) {
			t.Errorf("login page has inline %q, which the CSP refuses", bad)
		}
	}
}
