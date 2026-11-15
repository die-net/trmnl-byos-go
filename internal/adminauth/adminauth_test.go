package adminauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/apitoken"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

const testPassword = "correct horse battery"

// TestMain makes hashing cheap for the tests in this package.
func TestMain(m *testing.M) {
	hashParams = argonParams{memoryKiB: 64, passes: 1, threads: 1}
	os.Exit(m.Run())
}

var t0 = time.Unix(1_000_000, 0)

func openTest(t *testing.T) *db.Client {
	t.Helper()
	client, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func withPassword(t *testing.T) *db.Client {
	t.Helper()
	client := openTest(t)
	if err := SetPassword(t.Context(), client, testPassword, t0); err != nil {
		t.Fatal(err)
	}
	return client
}

func TestSetPasswordLength(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// minSetting is the min_password_length setting, or 0 to leave the
		// default of 12.
		minSetting int
		pw         string
		ok         bool
	}{
		{"default, empty", 0, "", false},
		{"default, 11 bytes", 0, strings.Repeat("a", 11), false},
		{"default, 12 bytes", 0, strings.Repeat("a", 12), true},
		{"default, 1024 bytes", 0, strings.Repeat("a", 1024), true},
		{"default, 1025 bytes", 0, strings.Repeat("a", 1025), false},
		{"set to 6, 5 bytes", 6, strings.Repeat("a", 5), false},
		{"set to 6, 6 bytes", 6, strings.Repeat("a", 6), true},
		{"set to 20, 19 bytes", 20, strings.Repeat("a", 19), false},
		{"set to 20, 20 bytes", 20, strings.Repeat("a", 20), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := openTest(t)
			if tc.minSetting != 0 {
				if err := settings.Set(t.Context(), client, settings.KeyMinPasswordLength, strconv.Itoa(tc.minSetting), t0); err != nil {
					t.Fatal(err)
				}
			}
			err := SetPassword(t.Context(), client, tc.pw, t0)
			if tc.ok && err != nil {
				t.Fatal(err)
			}
			if !tc.ok {
				if !errors.Is(err, errkind.ErrInvalid) || !errors.Is(err, ErrPasswordLength) {
					t.Fatalf("err = %v, want a password length error", err)
				}
				wantMin := 12
				if tc.minSetting != 0 {
					wantMin = tc.minSetting
				}
				if want := fmt.Sprintf("must be %d to 1024 bytes", wantMin); !strings.Contains(err.Error(), want) {
					t.Errorf("err %q does not state the bounds %q", err, want)
				}
			}
		})
	}
}

// TestRaisedMinimumKeepsOldPassword checks that the minimum applies only
// when a password is set: one set under a lower minimum still logs in.
func TestRaisedMinimumKeepsOldPassword(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := openTest(t)
	if err := settings.Set(ctx, client, settings.KeyMinPasswordLength, "6", t0); err != nil {
		t.Fatal(err)
	}
	if err := SetPassword(ctx, client, "sixsix", t0); err != nil {
		t.Fatal(err)
	}
	if err := settings.Set(ctx, client, settings.KeyMinPasswordLength, "30", t0); err != nil {
		t.Fatal(err)
	}
	if err := CheckPassword(ctx, client, "sixsix"); err != nil {
		t.Fatalf("old password after raising the minimum: %v", err)
	}
	if err := SetPassword(ctx, client, "sixsix", t0); !errors.Is(err, ErrPasswordLength) {
		t.Fatalf("setting it again: %v, want a length error", err)
	}
}

// TestSetPasswordCorruptMinimum checks that an unusable setting is an error
// naming the key, not a silent default.
func TestSetPasswordCorruptMinimum(t *testing.T) {
	t.Parallel()
	client := openTest(t)
	if err := client.Write.SetSetting(t.Context(), db.SetSettingParams{Key: settings.KeyMinPasswordLength, Value: "lots", UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	err := SetPassword(t.Context(), client, testPassword, t0)
	if err == nil || !strings.Contains(err.Error(), settings.KeyMinPasswordLength) {
		t.Fatalf("err = %v, want one naming the setting", err)
	}
}

func TestCheckPassword(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := withPassword(t)
	for _, tc := range []struct {
		name string
		pw   string
		want error
	}{
		{"right", testPassword, nil},
		{"wrong", testPassword + "x", ErrWrongPassword},
		{"empty", "", ErrWrongPassword},
		{"too long", strings.Repeat("a", settings.MaxPasswordLen+1), ErrWrongPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := CheckPassword(ctx, client, tc.pw); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if err := CheckPassword(ctx, openTest(t), testPassword); !errors.Is(err, ErrNoPassword) {
		t.Fatalf("with no password set: %v, want ErrNoPassword", err)
	}
}

// TestHashUsesStoredParams checks that a hash made with one set of settings
// still verifies after the settings for new hashes change.
func TestHashUsesStoredParams(t *testing.T) {
	t.Parallel()
	phc, err := hashPassword(t.Context(), testPassword, hashParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash %q is not a PHC string with the test settings", phc)
	}
	if ok, err := verifyHash(t.Context(), phc, testPassword); err != nil || !ok {
		t.Fatalf("verify = %v, %v, want true", ok, err)
	}
	if ok, err := verifyHash(t.Context(), phc, "another password"); err != nil || ok {
		t.Fatalf("verify wrong = %v, %v, want false", ok, err)
	}
	phc2, err := hashPassword(t.Context(), testPassword, argonParams{memoryKiB: 128, passes: 2, threads: 2})
	if err != nil || phc2 == phc || !strings.Contains(phc2, "m=128,t=2,p=2") {
		t.Fatalf("second hash %q, %v: want a new salt and the new settings", phc2, err)
	}
}

func TestBadStoredHash(t *testing.T) {
	t.Parallel()
	good, err := hashPassword(t.Context(), testPassword, hashParams)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, "$")
	with := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return strings.Join(p, "$")
	}
	for _, tc := range []struct {
		name string
		phc  string
	}{
		{"empty", ""},
		{"plain text", "hunter2"},
		{"wrong algorithm", with(1, "argon2i")},
		{"wrong version", with(2, "v=16")},
		{"too few fields", strings.Join(parts[:5], "$")},
		{"memory over the bound", with(3, "m=4194304,t=1,p=1")},
		{"memory overflows 32 bits", with(3, "m=99999999999,t=1,p=1")},
		{"passes over the bound", with(3, "m=64,t=11,p=1")},
		{"zero passes", with(3, "m=64,t=0,p=1")},
		{"threads over the bound", with(3, "m=64,t=1,p=5")},
		{"threads overflow a byte", with(3, "m=64,t=1,p=256")},
		{"memory under 8 per thread", with(3, "m=7,t=1,p=1")},
		{"trailing text in settings", with(3, "m=64,t=1,p=1,x=2")},
		{"leading zero", with(3, "m=064,t=1,p=1")},
		{"salt not base64", with(4, "!!!!")},
		{"salt too short", with(4, "AAAA")},
		{"key not base64", with(5, "!!!!")},
		{"key too short", with(5, "AAAA")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := openTest(t)
			err := client.Write.SetSetting(t.Context(), db.SetSettingParams{Key: settingPasswordHash, Value: tc.phc, UpdatedAt: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckPassword(t.Context(), client, testPassword); !errors.Is(err, ErrBadHash) {
				t.Fatalf("err = %v, want ErrBadHash", err)
			}
			if _, err := Login(t.Context(), client, testPassword, t0); !errors.Is(err, ErrBadHash) {
				t.Fatalf("login err = %v, want ErrBadHash", err)
			}
		})
	}
}

func TestLoginWrongOrUnset(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := withPassword(t)
	if tok, err := Login(ctx, client, "wrong password!", t0); !errors.Is(err, ErrWrongPassword) || tok != "" {
		t.Fatalf("wrong password: %q, %v", tok, err)
	}
	if _, err := Login(ctx, openTest(t), testPassword, t0); !errors.Is(err, ErrNoPassword) {
		t.Fatalf("no password: %v", err)
	}
	var n int
	if err := client.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_session`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d sessions after failed logins, %v", n, err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := withPassword(t)
	tok, err := Login(ctx, client, testPassword, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 43 {
		t.Errorf("token %q has %d characters, want 43 (32 bytes, base64url)", tok, len(tok))
	}
	// Only the hash is stored.
	var stored []byte
	if err := client.Reader.QueryRowContext(ctx, `SELECT token_hash FROM admin_session`).Scan(&stored); err != nil || !bytes.Equal(stored, apitoken.Hash(tok)) {
		t.Fatalf("stored hash %x, %v", stored, err)
	}

	valid := func(token string, at time.Time) bool {
		t.Helper()
		ok, err := ValidSession(ctx, client, token, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if valid("", t0) || valid("nope", t0) {
		t.Error("a missing or unknown token is valid")
	}
	if !valid(tok, t0.Add(time.Second)) {
		t.Fatal("new session is not valid")
	}

	// Sliding: a use over a minute after the last moves the expiry out.
	use := t0.Add(20 * 24 * time.Hour)
	if !valid(tok, use) {
		t.Fatal("session not valid on day 20")
	}
	var seen, expires int64
	row := client.Reader.QueryRowContext(ctx, `SELECT last_seen_at, expires_at FROM admin_session`)
	if err := row.Scan(&seen, &expires); err != nil || seen != use.Unix() || expires != use.Add(Lifetime).Unix() {
		t.Fatalf("after use: seen %d expires %d, %v; want %d and %d", seen, expires, err, use.Unix(), use.Add(Lifetime).Unix())
	}
	// A use within the minute writes nothing.
	if !valid(tok, use.Add(30*time.Second)) {
		t.Fatal("session not valid 30 seconds later")
	}
	row = client.Reader.QueryRowContext(ctx, `SELECT last_seen_at, expires_at FROM admin_session`)
	if err := row.Scan(&seen, &expires); err != nil || seen != use.Unix() {
		t.Fatalf("a use inside a minute changed last_seen_at to %d, %v", seen, err)
	}
	// Day 20 pushed the end to day 50, so day 49 is valid, and it slid again.
	if !valid(tok, t0.Add(49*24*time.Hour)) {
		t.Fatal("session not valid on day 49 after a use on day 20")
	}
	// With no use, the end is a lifetime after the last one.
	end := t0.Add(49*24*time.Hour + Lifetime)
	if valid(tok, end) {
		t.Fatal("session valid at its expiry")
	}
	if valid(tok, end.Add(-time.Second)) != true {
		t.Fatal("session not valid a second before its expiry")
	}
}

func TestSessionExpires(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := withPassword(t)
	tok, err := Login(ctx, client, testPassword, t0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ValidSession(ctx, client, tok, t0.Add(Lifetime)); err != nil || ok {
		t.Fatalf("session at its expiry: %v, %v", ok, err)
	}
	// A later login deletes the expired row.
	if _, err := Login(ctx, client, testPassword, t0.Add(Lifetime+time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := client.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_session`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d sessions after login, want 1: %v", n, err)
	}
}

func TestLoginMakesNewToken(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := withPassword(t)
	a, err := Login(ctx, client, testPassword, t0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Login(ctx, client, testPassword, t0)
	if err != nil || a == b {
		t.Fatalf("two logins gave %q and %q, %v", a, b, err)
	}
}

func TestLogoutAndSetPassword(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := withPassword(t)
	a, _ := Login(ctx, client, testPassword, t0)
	b, _ := Login(ctx, client, testPassword, t0)
	valid := func(tok string) bool {
		ok, err := ValidSession(ctx, client, tok, t0)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if err := Logout(ctx, client, a); err != nil {
		t.Fatal(err)
	}
	if valid(a) || !valid(b) {
		t.Fatalf("after logout of a: a valid %v, b valid %v", valid(a), valid(b))
	}
	if err := Logout(ctx, client, "unknown"); err != nil {
		t.Fatalf("logout of an unknown token: %v", err)
	}
	if err := SetPassword(ctx, client, "a new password here", t0); err != nil {
		t.Fatal(err)
	}
	if valid(b) {
		t.Fatal("session survived SetPassword")
	}
	if err := CheckPassword(ctx, client, testPassword); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("old password: %v", err)
	}
	if err := CheckPassword(ctx, client, "a new password here"); err != nil {
		t.Fatalf("new password: %v", err)
	}
}

func TestSignedIn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := withPassword(t)
	tok, err := Login(ctx, client, testPassword, t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		cookie string
		want   bool
	}{
		{"valid", tok, true},
		{"unknown", "abc", false},
		{"none", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			if tc.cookie != "" {
				r.Header.Set("Cookie", CookieName+"="+tc.cookie)
			}
			if got, err := SignedIn(ctx, client, r, t0); err != nil || got != tc.want {
				t.Fatalf("SignedIn = %v, %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestCookie(t *testing.T) {
	t.Parallel()
	for _, secure := range []bool{false, true} {
		c := NewCookie("tok", secure)
		if c.Name != "trmnl_session" || c.Value != "tok" || c.Path != "/" || !c.HttpOnly ||
			c.SameSite != http.SameSiteLaxMode || c.Secure != secure || c.MaxAge != 30*24*3600 {
			t.Errorf("secure=%v: cookie %+v", secure, c)
		}
		if c := ClearCookie(secure); c.MaxAge >= 0 || c.Value != "" || c.Secure != secure {
			t.Errorf("secure=%v: clear cookie %+v", secure, c)
		}
	}
}

// TestHashWaitsForSlot checks that a login waiting for the one hashing slot
// returns when its context is canceled, and that the slot is free again once
// the holder is done.
func TestHashWaitsForSlot(t *testing.T) {
	t.Parallel()
	client := withPassword(t)
	hashSlot <- struct{}{}
	held := true
	release := func() {
		if held {
			<-hashSlot
			held = false
		}
	}
	t.Cleanup(release)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := Login(ctx, client, testPassword, t0)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("login returned %v while the slot was held", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("login still waiting after its context was canceled")
	}

	release()
	if _, err := Login(t.Context(), client, testPassword, t0); err != nil {
		t.Fatalf("login after the slot was freed: %v", err)
	}
}
