package imagestore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nameOf is the name an image with these bytes is stored under.
func nameOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:nameLen]
}

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
	name, _, err := s.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(name) != nameLen || name != nameOf(data) {
		t.Fatalf("name %q, want %d hex digits matching its hash", name, nameLen)
	}
	if again, size, err := s.Put(bytes.NewReader(data)); err != nil || again != name || size != int64(len(data)) {
		t.Fatalf("second Put = %q, %d, %v; want %q, %d", again, size, err, name, len(data))
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
	name, _, err := s.Put(strings.NewReader("x"))
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
	name, _, err := s.Put(bytes.NewReader(data))
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
		if _, _, err := s.Put(bytes.NewReader(data)); err != nil {
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
		name, _, err := s.Put(strings.NewReader(data))
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
	stray := filepath.Join(dir, tmpPrefix+"123")
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

// TestCommitKeepsExisting checks that storing bytes already stored leaves
// the existing file in place rather than replacing it, which keeps
// filesystem snapshots from holding a copy of every rewrite, and leaves no
// temporary file behind.
func TestCommitKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	name, _, err := s.Put(strings.NewReader("image"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(s.path(name))
	if err != nil {
		t.Fatal(err)
	}
	if again, _, err := s.Put(strings.NewReader("image")); err != nil || again != name {
		t.Fatalf("second Put = %q, %v", again, err)
	}
	after, err := os.Stat(s.path(name))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("storing the same bytes replaced the file")
	}
	assertNoTemp(t, dir)
}

// TestAbort checks that an aborted image leaves nothing behind.
func TestAbort(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	w.Abort()
	w.Abort() // A second Abort does nothing.
	if _, _, err := w.Commit(); err == nil {
		t.Error("Commit after Abort succeeded")
	}
	assertNoTemp(t, dir)
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
}

// TestCommitSyncsDirectory checks what a test can about the directory sync
// after the rename: Commit works, a new image's shard directory is synced
// too, and an error from opening the directory is returned and leaves no
// file under the name, so a retry syncs again. A power loss cannot be
// tested portably, so that the sync reaches the disk is not.
func TestCommitSyncsDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory, so the open cannot fail")
	}
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(strings.NewReader("ok")); err != nil {
		t.Fatalf("Put = %v", err)
	}
	// A shard directory that can be written and searched, but not opened to
	// read, so the rename works and the open for the sync fails.
	const data = "unsyncable"
	name := nameOf([]byte(data))
	shard := filepath.Dir(s.path(name))
	// It is left empty, so the temporary directory can still be removed.
	if err := os.Mkdir(shard, 0o300); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(strings.NewReader(data)); err == nil {
		t.Fatal("Put succeeded although the directory could not be synced")
	}
	if _, err := os.Stat(s.path(name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image kept after a failed sync: %v", err)
	}
	assertNoTemp(t, dir)
}

// TestSyncDir checks that syncDir works on a directory and returns the error
// for one that cannot be opened.
func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir on a directory = %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("syncDir on a missing directory succeeded")
	}
}
