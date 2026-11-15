package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func newKey(t *testing.T) *Key {
	t.Helper()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	k, err := Parse(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpen(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	sealed, err := k.Seal([]byte("hunter2"), []byte("1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("sealed value holds the plaintext")
	}
	got, err := k.Open(sealed, []byte("1"))
	if err != nil || string(got) != "hunter2" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := newKey(t).Open(sealed, []byte("1")); err == nil {
		t.Error("another key opened the value")
	}
	if _, err := k.Open(sealed, []byte("2")); err == nil {
		t.Error("other associated data opened the value")
	}
	if _, err := k.Open(sealed, nil); err == nil {
		t.Error("missing associated data opened the value")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := k.Open(sealed, []byte("1")); err == nil {
		t.Error("a changed value opened")
	}
}

func TestBadKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, key string }{
		{"not base64", "!!!"},
		{"short", base64.StdEncoding.EncodeToString(make([]byte, 16))},
	} {
		if _, err := Parse(tc.key); err == nil {
			t.Errorf("%s: Parse succeeded", tc.name)
		}
	}
	var none *Key
	if _, err := none.Seal([]byte("x"), nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("nil key Seal: %v", err)
	}
	if _, err := none.Open([]byte("x"), nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("nil key Open: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(EnvVar, "")
	if k, err := FromEnv(); k != nil || err != nil {
		t.Errorf("unset: %v, %v", k, err)
	}
	t.Setenv(EnvVar, "short")
	if _, err := FromEnv(); err == nil {
		t.Error("bad key accepted")
	}
}
