package db

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
)

func openTest(t *testing.T) *Client {
	t.Helper()
	client, err := Open(t.Context(), filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestOpenPools checks that the writer runs WAL with synchronous=NORMAL and
// foreign keys on, and that the reader pool refuses writes.
func TestOpenPools(t *testing.T) {
	ctx := t.Context()
	client := openTest(t)

	var mode string
	if err := client.Reader.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("reader journal_mode = %q, %v; want wal", mode, err)
	}
	var sync int
	if err := client.Writer.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil || sync != 1 {
		t.Fatalf("writer synchronous = %d, %v; want 1 (NORMAL)", sync, err)
	}
	var fk int
	if err := client.Writer.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("writer foreign_keys = %d, %v; want 1", fk, err)
	}
	set := SetSettingParams{Key: "k", Value: "v", UpdatedAt: 1}
	if err := client.Read.SetSetting(ctx, set); err == nil {
		t.Fatal("reader accepted a write")
	}
	if err := client.Write.SetSetting(ctx, set); err != nil {
		t.Fatalf("writer: %v", err)
	}
	if v, err := client.Read.GetSetting(ctx, "k"); err != nil || v != "v" {
		t.Fatalf("GetSetting = %q, %v; want v", v, err)
	}
}

// TestReopen checks that opening an existing database applies the schema
// again without error and keeps its rows.
func TestReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "trmnl.db")
	for i := range 2 {
		client, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if i == 0 {
			err = client.Write.SetSetting(ctx, SetSettingParams{Key: "k", Value: "v", UpdatedAt: 1})
		} else {
			_, err = client.Read.GetSetting(ctx, "k")
		}
		if cerr := client.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
}

// TestInTx checks that InTx commits what fn wrote when it returns nil, and
// rolls all of it back when fn fails.
func TestInTx(t *testing.T) {
	ctx := t.Context()
	client := openTest(t)

	insert := func(mac string) func(q *Queries) error {
		return func(q *Queries) error {
			_, err := q.CreateDevice(ctx, CreateDeviceParams{Mac: mac, ApiKey: "key-" + mac, FriendlyID: "id-" + mac, CreatedAt: 1})
			return err
		}
	}
	count := func() int {
		t.Helper()
		devices, err := client.Read.ListDevices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(devices)
	}
	boom := errors.New("boom")
	err := client.InTx(ctx, func(q *Queries) error {
		if err := insert("a")(q); err != nil {
			return err
		}
		if err := insert("b")(q); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx = %v, want boom", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d rows after a rollback", n)
	}
	if err := client.InTx(ctx, insert("a")); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("%d rows after a commit, want 1", n)
	}
}

// TestWriterSerializes checks that concurrent InTx calls queue for the one
// writer connection rather than failing with SQLITE_BUSY.
func TestWriterSerializes(t *testing.T) {
	ctx := t.Context()
	client := openTest(t)

	const n = 32
	errs := make(chan error, n)
	for i := range n {
		go func() {
			errs <- client.InTx(ctx, func(q *Queries) error {
				mac := strconv.Itoa(i)
				_, err := q.CreateDevice(ctx, CreateDeviceParams{Mac: mac, ApiKey: "key-" + mac, FriendlyID: "id-" + mac, CreatedAt: 1})
				return err
			})
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	devices, err := client.Read.ListDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != n {
		t.Fatalf("%d devices, want %d", len(devices), n)
	}
}
