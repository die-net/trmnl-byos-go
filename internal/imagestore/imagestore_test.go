package imagestore

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// serve makes a request to the store's handler.
func serve(t *testing.T, s *Store, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("GET /images/{name}", s)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
	return rec
}

// TestPutAndServe checks that an image is stored under a directory named
// for the first two hex digits of its name, that storing it again keeps the
// name, and that it is served back unchanged.
func TestPutAndServe(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("\x89PNG not really")
	name, err := s.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(name) != nameLen || name != Name(data) {
		t.Fatalf("name %q, want %d hex digits matching Name", name, nameLen)
	}
	if again, err := s.Put(data); err != nil || again != name {
		t.Fatalf("second Put = %q, %v; want %q", again, err, name)
	}
	onDisk, err := fs.ReadFile(os.DirFS(dir), name[:2]+"/"+name[2:]+".png")
	if err != nil || !bytes.Equal(onDisk, data) {
		t.Fatalf("file on disk = %q, %v", onDisk, err)
	}

	rec := serve(t, s, "/images/"+name+".png")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("GET = %d %q", rec.Code, rec.Body.Bytes())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type %q", ct)
	}
}

// TestServeRejects checks that only well-formed names of stored images are
// served.
func TestServeRejects(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name, err := s.Put([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/images/" + name,                      // no extension
		"/images/" + name + ".gif",             // wrong extension
		"/images/" + name[:nameLen-1] + ".png", // too short
		"/images/" + name + "0.png",            // too long
		"/images/ABCDEF0123456789.png",         // not lowercase hex
		"/images/0123456789abcdef.png",         // not stored
		"/images/..%2F..%2Fetc%2Fpasswd.png",   // path traversal
	} {
		if rec := serve(t, s, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// TestPutTouches checks that storing existing bytes again leaves the file's
// contents alone and refreshes its time only once it is touchAfter old.
func TestPutTouches(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("image")
	name, err := s.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	path := s.path(name)
	for _, tc := range []struct {
		age     time.Duration
		touched bool
	}{
		{touchAfter / 2, false},
		{2 * touchAfter, true},
	} {
		old := time.Now().Add(-tc.age).Truncate(time.Second)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(data); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if touched := !info.ModTime().Equal(old); touched != tc.touched {
			t.Errorf("age %v: touched = %v, want %v", tc.age, touched, tc.touched)
		}
	}
}

// TestExpire checks that Expire deletes only old images that keep does not
// name, plus old leftovers from a crashed Put, and leaves everything else.
func TestExpire(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cutoff := now.Add(-24 * time.Hour)
	old := cutoff.Add(-time.Hour)
	put := func(data string, mtime time.Time) string {
		t.Helper()
		name, err := s.Put([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(s.path(name), mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return name
	}
	oldUnused := put("old unused", old)
	oldKept := put("old kept", old)
	fresh := put("fresh", now)
	stray := filepath.Join(dir, oldKept[:2], tmpPrefix+"123")
	other := filepath.Join(dir, oldKept[:2], "notes.txt")
	for _, f := range []string{stray, other} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.Expire(map[string]bool{oldKept: true}, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("Expire = %d, %v; want 1", n, err)
	}
	for _, tc := range []struct {
		path   string
		exists bool
	}{
		{s.path(oldUnused), false},
		{s.path(oldKept), true},
		{s.path(fresh), true},
		{stray, false},
		{other, true},
	} {
		_, err := os.Stat(tc.path)
		if exists := err == nil; exists != tc.exists {
			t.Errorf("%s exists = %v, want %v", tc.path, exists, tc.exists)
		}
	}
}
