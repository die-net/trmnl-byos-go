package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// runSetting runs `setting args...` and returns its output.
func runSetting(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := setting(t.Context(), args, &out)
	return out.String(), err
}

func TestSetting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	list := func() map[string][]string {
		t.Helper()
		out, err := runSetting(t, "list", "-data", dir)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if got := strings.Fields(lines[0]); strings.Join(got, " ") != "KEY VALUE DEFAULT IS DEFAULT" {
			t.Fatalf("header %q", lines[0])
		}
		rows := map[string][]string{}
		for _, l := range lines[1:] {
			f := strings.Fields(l)
			rows[f[0]] = f[1:]
		}
		return rows
	}
	same := func(got []string, want ...string) bool { return strings.Join(got, " ") == strings.Join(want, " ") }

	rows := list()
	if !same(rows["refresh_seconds"], "900", "900", "yes") || !same(rows["min_password_length"], "12", "12", "yes") || len(rows) != 2 {
		t.Fatalf("defaults: %v", rows)
	}
	if _, err := runSetting(t, "set", "-data", dir, "refresh_seconds", "600"); err != nil {
		t.Fatal(err)
	}
	if rows := list(); !same(rows["refresh_seconds"], "600", "900", "no") || !same(rows["min_password_length"], "12", "12", "yes") {
		t.Fatalf("after set: %v", rows)
	}
	if _, err := runSetting(t, "reset", "-data", dir, "refresh_seconds"); err != nil {
		t.Fatal(err)
	}
	if rows := list(); !same(rows["refresh_seconds"], "900", "900", "yes") {
		t.Fatalf("after reset: %v", rows)
	}
}

func TestSettingRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		kind error
	}{
		{"set out of range", []string{"set", "refresh_seconds", "0"}, errkind.ErrInvalid},
		{"set not a number", []string{"set", "refresh_seconds", "15m"}, errkind.ErrInvalid},
		{"set unknown key", []string{"set", "nope", "1"}, errkind.ErrNotFound},
		{"set join_until", []string{"set", "join_until", "1"}, errkind.ErrNotFound},
		{"set the password hash", []string{"set", "admin_password_hash", "x"}, errkind.ErrNotFound},
		{"reset unknown key", []string{"reset", "nope"}, errkind.ErrNotFound},
		{"reset join_until", []string{"reset", "join_until"}, errkind.ErrNotFound},
		{"set without a value", []string{"set", "refresh_seconds"}, nil},
		{"reset without a key", []string{"reset"}, nil},
		{"list with an argument", []string{"list", "extra"}, nil},
		{"no subcommand", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			args := tc.args
			if len(args) > 0 {
				args = append([]string{args[0], "-data", dir}, args[1:]...)
			}
			if _, err := runSetting(t, args...); err == nil || (tc.kind != nil && !errors.Is(err, tc.kind)) {
				t.Fatalf("err = %v, want kind %v", err, tc.kind)
			}
			// Nothing was written.
			out, err := runSetting(t, "list", "-data", dir)
			if err != nil || strings.Contains(out, "no\n") {
				t.Errorf("list after: %q, %v", out, err)
			}
		})
	}
}
