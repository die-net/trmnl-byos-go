package cache

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type header struct {
	Name string `json:"name"`
}

func put(t *testing.T, c *Cache, k Key, name, body string) {
	t.Helper()
	w, err := c.Create(k, header{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k := KeyOf("https://example.com/", "markdown")
	if KeyOf("ab", "c") == KeyOf("a", "bc") {
		t.Error("key parts run together")
	}
	var h header
	if _, err := c.Open(k, &h); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing entry: %v", err)
	}
	put(t, c, k, "first", "body\nwith lines\n")
	put(t, c, k, "second", "replaced")

	e, err := c.Open(k, &h)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	body, err := io.ReadAll(e)
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != "second" || string(body) != "replaced" {
		t.Errorf("got %q %q", h.Name, body)
	}
	s := k.String()
	if _, err := os.Stat(filepath.Join(c.dir, s[:2], s[2:])); err != nil {
		t.Errorf("entry not at hex path: %v", err)
	}
}

func TestAbort(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	k := KeyOf("x")
	w, err := c.Create(k, header{})
	if err != nil {
		t.Fatal(err)
	}
	w.Abort()
	entries, _ := os.ReadDir(filepath.Join(dir, k.String()[:2]))
	if len(entries) != 0 {
		t.Errorf("abort left %v", entries)
	}
}

func TestSweep(t *testing.T) {
	t.Parallel()
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old, recent, newest := KeyOf("old"), KeyOf("recent"), KeyOf("newest")
	put(t, c, old, "old", "x")
	put(t, c, recent, "recent", "0123456789")
	put(t, c, newest, "newest", "0123456789")
	if err := c.Touch(old, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := c.Touch(recent, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	removed, err := c.Sweep(now, time.Hour, 0)
	if err != nil || removed != 1 {
		t.Fatalf("age sweep removed %d, %v", removed, err)
	}
	var h header
	if _, err := c.Open(old, &h); !errors.Is(err, fs.ErrNotExist) {
		t.Error("old entry survived")
	}
	// Each entry is about 30 bytes; a 40-byte limit keeps only the newest.
	removed, err = c.Sweep(now, time.Hour, 40)
	if err != nil || removed != 1 {
		t.Fatalf("size sweep removed %d, %v", removed, err)
	}
	if _, err := c.Open(recent, &h); !errors.Is(err, fs.ErrNotExist) {
		t.Error("older entry survived the size sweep")
	}
	e, err := c.Open(newest, &h)
	if err != nil {
		t.Fatal("newest entry was removed")
	}
	_ = e.Close()
}
