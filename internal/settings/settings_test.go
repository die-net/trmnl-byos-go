package settings

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

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

// putRaw stores a value without validating it.
func putRaw(t *testing.T, client *db.Client, key, value string) {
	t.Helper()
	if err := client.Write.SetSetting(t.Context(), db.SetSettingParams{Key: key, Value: value, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	client := openTest(t)
	refresh, err := RefreshInterval(t.Context(), client.Read)
	if err != nil || refresh != 15*time.Minute {
		t.Errorf("RefreshInterval = %v, %v; want 15m", refresh, err)
	}
	minLen, err := MinPasswordLength(t.Context(), client.Read)
	if err != nil || minLen != 12 {
		t.Errorf("MinPasswordLength = %v, %v; want 12", minLen, err)
	}
	rows, err := List(t.Context(), client.Read)
	if err != nil || len(rows) != 2 {
		t.Fatalf("List = %v, %v", rows, err)
	}
	for _, r := range rows {
		if r.Set || r.Value != r.Default {
			t.Errorf("%+v: want the default, not set", r)
		}
	}
}

func TestSetAndGet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, key, value string
		want             int64
	}{
		{"refresh minimum", KeyRefreshSeconds, "1", 1},
		{"refresh typical", KeyRefreshSeconds, "600", 600},
		{"refresh maximum", KeyRefreshSeconds, "31536000", 31536000},
		{"min length minimum", KeyMinPasswordLength, "1", 1},
		{"min length typical", KeyMinPasswordLength, "20", 20},
		{"min length maximum", KeyMinPasswordLength, "1024", 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := openTest(t)
			if err := Set(t.Context(), client, tc.key, tc.value, t0); err != nil {
				t.Fatal(err)
			}
			rows, err := List(t.Context(), client.Read)
			if err != nil {
				t.Fatal(err)
			}
			var got *Setting
			for i := range rows {
				if rows[i].Key == tc.key {
					got = &rows[i]
				}
			}
			if got == nil || got.Value != tc.want || !got.Set {
				t.Fatalf("List row = %+v, want value %d, set", got, tc.want)
			}
			var typed int64
			switch tc.key {
			case KeyRefreshSeconds:
				d, err := RefreshInterval(t.Context(), client.Read)
				if err != nil {
					t.Fatal(err)
				}
				typed = int64(d / time.Second)
			case KeyMinPasswordLength:
				n, err := MinPasswordLength(t.Context(), client.Read)
				if err != nil {
					t.Fatal(err)
				}
				typed = int64(n)
			}
			if typed != tc.want {
				t.Errorf("typed getter = %d, want %d", typed, tc.want)
			}
		})
	}
}

func TestSetRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, key, value string
		kind             error
	}{
		{"refresh zero", KeyRefreshSeconds, "0", errkind.ErrInvalid},
		{"refresh negative", KeyRefreshSeconds, "-5", errkind.ErrInvalid},
		{"refresh over a year", KeyRefreshSeconds, "31536001", errkind.ErrInvalid},
		{"refresh overflows int64", KeyRefreshSeconds, "99999999999999999999", errkind.ErrInvalid},
		{"min length zero", KeyMinPasswordLength, "0", errkind.ErrInvalid},
		{"min length over the maximum", KeyMinPasswordLength, "1025", errkind.ErrInvalid},
		{"words", KeyRefreshSeconds, "soon", errkind.ErrInvalid},
		{"duration", KeyRefreshSeconds, "15m", errkind.ErrInvalid},
		{"fraction", KeyRefreshSeconds, "1.5", errkind.ErrInvalid},
		{"empty", KeyMinPasswordLength, "", errkind.ErrInvalid},
		{"unknown key", "nope", "1", errkind.ErrNotFound},
		{"join window", "join_until", "1", errkind.ErrNotFound},
		{"password hash", "admin_password_hash", "x", errkind.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := openTest(t)
			err := Set(t.Context(), client, tc.key, tc.value, t0)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("err = %v, want kind %v", err, tc.kind)
			}
			if errors.Is(tc.kind, errkind.ErrInvalid) && !strings.Contains(err.Error(), tc.key) {
				t.Errorf("err %q does not name the key", err)
			}
			if _, err := client.Read.GetSetting(t.Context(), tc.key); err == nil {
				t.Errorf("a row for %s was written", tc.key)
			}
		})
	}
}

func TestSetManyWritesNothingOnABadChange(t *testing.T) {
	t.Parallel()
	client := openTest(t)
	err := SetMany(t.Context(), client, []Change{{KeyRefreshSeconds, "60"}, {KeyMinPasswordLength, "0"}}, t0)
	if !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	rows, err := List(t.Context(), client.Read)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Set {
			t.Errorf("%s was written", r.Key)
		}
	}
}

func TestReset(t *testing.T) {
	t.Parallel()
	client := openTest(t)
	if err := Set(t.Context(), client, KeyRefreshSeconds, "60", t0); err != nil {
		t.Fatal(err)
	}
	if err := Reset(t.Context(), client, KeyRefreshSeconds); err != nil {
		t.Fatal(err)
	}
	d, err := RefreshInterval(t.Context(), client.Read)
	if err != nil || d != 15*time.Minute {
		t.Fatalf("after reset: %v, %v", d, err)
	}
	// Resetting a setting with no row is not an error.
	if err := Reset(t.Context(), client, KeyRefreshSeconds); err != nil {
		t.Errorf("second reset: %v", err)
	}
	if err := Reset(t.Context(), client, "nope"); !errors.Is(err, errkind.ErrNotFound) {
		t.Errorf("unknown key: %v", err)
	}
}

func TestCorruptValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, key, value string }{
		{"not a number", KeyRefreshSeconds, "soon"},
		{"zero", KeyRefreshSeconds, "0"},
		{"over the maximum", KeyMinPasswordLength, "5000"},
		{"empty", KeyMinPasswordLength, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := openTest(t)
			putRaw(t, client, tc.key, tc.value)
			var err error
			var def int64
			if tc.key == KeyRefreshSeconds {
				var d time.Duration
				d, err = RefreshInterval(t.Context(), client.Read)
				def = int64(d / time.Second)
			} else {
				var n int
				n, err = MinPasswordLength(t.Context(), client.Read)
				def = int64(n)
			}
			// A row that is not usable is not an error: the default is in
			// effect, as List reports.
			if err != nil {
				t.Fatalf("err = %v, want none", err)
			}
			want := int64(900)
			if tc.key == KeyMinPasswordLength {
				want = 12
			}
			if def != want {
				t.Errorf("value = %d, want the default %d", def, want)
			}
			// A reset repairs it.
			if err := Reset(t.Context(), client, tc.key); err != nil {
				t.Fatal(err)
			}
			if _, err := RefreshInterval(t.Context(), client.Read); err != nil {
				t.Errorf("RefreshInterval after reset: %v", err)
			}
		})
	}
}

// TestListBadRow checks that List reports a row that is not usable, and
// that Set and Reset each repair it.
func TestListBadRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, key, value string
	}{
		{"not a number", KeyRefreshSeconds, "soon"},
		{"zero", KeyRefreshSeconds, "0"},
		{"over the maximum", KeyMinPasswordLength, "5000"},
		{"empty", KeyMinPasswordLength, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := openTest(t)
			putRaw(t, client, tc.key, tc.value)
			check := func(wantBad bool) {
				t.Helper()
				rows, err := List(t.Context(), client.Read)
				if err != nil || len(rows) != 2 {
					t.Fatalf("List = %v, %v", rows, err)
				}
				for _, r := range rows {
					switch {
					case r.Key == tc.key && wantBad:
						if r.Bad == nil || *r.Bad != tc.value || !r.Set || r.Value != r.Default {
							t.Errorf("%+v: want bad %q, set, the default in effect", r, tc.value)
						}
					case r.Bad != nil || r.Value != r.Default || (r.Set && r.Key != tc.key):
						t.Errorf("%+v: want a clean setting at its default", r)
					}
				}
			}
			check(true)
			// The readers the server uses agree with List: the default,
			// and no error.
			if d, err := RefreshInterval(t.Context(), client.Read); err != nil || d != 900*time.Second {
				t.Errorf("RefreshInterval = %v, %v; want the default and no error", d, err)
			}
			if n, err := MinPasswordLength(t.Context(), client.Read); err != nil || n != 12 {
				t.Errorf("MinPasswordLength = %d, %v; want the default and no error", n, err)
			}
			if err := Reset(t.Context(), client, tc.key); err != nil {
				t.Fatal(err)
			}
			check(false)
			putRaw(t, client, tc.key, tc.value)
			if err := Set(t.Context(), client, tc.key, "20", t0); err != nil {
				t.Fatal(err)
			}
			rows, err := List(t.Context(), client.Read)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.Key == tc.key && (r.Bad != nil || !r.Set || r.Value != 20) {
					t.Errorf("after Set: %+v", r)
				}
			}
		})
	}
}

// TestListOneBadRow checks that a bad row does not hide a good one.
func TestListOneBadRow(t *testing.T) {
	t.Parallel()
	client := openTest(t)
	putRaw(t, client, KeyRefreshSeconds, "abc")
	if err := Set(t.Context(), client, KeyMinPasswordLength, "30", t0); err != nil {
		t.Fatal(err)
	}
	rows, err := List(t.Context(), client.Read)
	if err != nil || len(rows) != 2 {
		t.Fatalf("List = %v, %v", rows, err)
	}
	if r := rows[0]; r.Bad == nil || *r.Bad != "abc" || r.Value != 900 || !r.Set {
		t.Errorf("refresh: %+v", r)
	}
	if r := rows[1]; r.Bad != nil || r.Value != 30 || !r.Set {
		t.Errorf("min length: %+v", r)
	}
}

// TestOtherRowsAreNotExposed checks that the join window and the admin
// password hash can be neither listed nor set here, and are left alone.
func TestOtherRowsAreNotExposed(t *testing.T) {
	t.Parallel()
	client := openTest(t)
	const hash = "$argon2id$secret"
	putRaw(t, client, "admin_password_hash", hash)
	if err := device.SetJoinWindow(t.Context(), client, t0); err != nil {
		t.Fatal(err)
	}
	rows, err := List(t.Context(), client.Read)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Key == "join_until" || r.Key == "admin_password_hash" || strconv.FormatInt(r.Value, 10) == hash {
			t.Errorf("List shows %+v", r)
		}
	}
	for _, key := range []string{"join_until", "admin_password_hash"} {
		if _, err := getKey(t.Context(), client.Read, key); !errors.Is(err, errkind.ErrNotFound) {
			t.Errorf("get %s: %v, want not found", key, err)
		}
		if err := Set(t.Context(), client, key, "1", t0); !errors.Is(err, errkind.ErrNotFound) {
			t.Errorf("set %s: %v, want not found", key, err)
		}
		if err := Reset(t.Context(), client, key); !errors.Is(err, errkind.ErrNotFound) {
			t.Errorf("reset %s: %v, want not found", key, err)
		}
	}
	if got, err := client.Read.GetSetting(t.Context(), "admin_password_hash"); err != nil || got != hash {
		t.Errorf("hash row = %q, %v; want it untouched", got, err)
	}
	if _, err := client.Read.GetSetting(t.Context(), "join_until"); err != nil {
		t.Errorf("join_until row: %v", err)
	}
}

// TestInTx checks that the getters read through a transaction's queries.
func TestInTx(t *testing.T) {
	t.Parallel()
	client := openTest(t)
	err := client.InTx(t.Context(), func(q *db.Queries) error {
		d, err := RefreshInterval(t.Context(), q)
		if err != nil || d != 15*time.Minute {
			t.Errorf("RefreshInterval = %v, %v", d, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
