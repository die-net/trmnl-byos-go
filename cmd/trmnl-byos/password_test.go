package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/adminauth"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// stdin returns a file, which is not a terminal, holding s.
func stdin(t *testing.T, s string) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPasswordFromStdin(t *testing.T) {
	const pw = "correct horse battery"
	for _, tc := range []struct {
		name  string
		input string
		want  string
		bad   bool
	}{
		{"line", pw + "\n", pw, false},
		{"CRLF", pw + "\r\n", pw, false},
		{"no newline", pw, pw, false},
		{"only the first line", pw + "\nsecond\n", pw, false},
		{"spaces are kept", " " + pw + " \n", " " + pw + " ", false},
		{"too short", "short\n", "", true},
		{"too long", strings.Repeat("a", settings.MaxPasswordLen+1) + "\n", "", true},
		{"empty", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var out bytes.Buffer
			err := password(t.Context(), []string{"-data", dir}, stdin(t, tc.input), &out)
			if tc.bad {
				if err == nil {
					t.Fatal("password accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "All sessions were signed out") || strings.Contains(out.String(), strings.TrimSpace(tc.want)) {
				t.Errorf("output %q should say sessions were signed out and not hold the password", out.String())
			}
			client, err := openDB(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if err := adminauth.CheckPassword(t.Context(), client, tc.want); err != nil {
				t.Errorf("stored password does not match %q: %v", tc.want, err)
			}
		})
	}
}

func TestPasswordTooShortIsInvalid(t *testing.T) {
	err := password(t.Context(), []string{"-data", t.TempDir()}, stdin(t, "short\n"), &bytes.Buffer{})
	if !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("err = %v, want invalid input", err)
	}
}
