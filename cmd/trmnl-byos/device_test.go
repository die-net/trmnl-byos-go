package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	_ "time/tzdata" // So the zones the tests name load on any host.

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// runDevice runs `device args...` and returns its output.
func runDevice(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := devices(t.Context(), append(args[:1:1], append([]string{"-data", dir}, args[1:]...)...), &out)
	return out.String(), err
}

// deviceDir returns a data directory with devices ABC001 and ABC002.
func deviceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	client, err := openDB(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, d := range []db.CreateDeviceParams{
		{Mac: "D8:3B:DA:12:34:01", ApiKeyHash: []byte("key 1"), FriendlyID: "ABC001", CreatedAt: 1},
		{Mac: "D8:3B:DA:12:34:02", ApiKeyHash: []byte("key 2"), FriendlyID: "ABC002", CreatedAt: 1},
	} {
		if _, err := client.Write.CreateDevice(t.Context(), d); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// listRows runs `device list` or `device blocked` and returns each data
// row's fields, keyed by its first field.
func listRows(t *testing.T, dir, cmd string) map[string][]string {
	t.Helper()
	out, err := runDevice(t, dir, cmd)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	rows := map[string][]string{}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		rows[f[0]] = f
	}
	return rows
}

func TestDeviceDelete(t *testing.T) {
	t.Parallel()
	dir := deviceDir(t)
	out, err := runDevice(t, dir, "delete", "abc001")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Deleted ABC001 (D8:3B:DA:12:34:01). Its next request resets it into setup.\n"; out != want {
		t.Errorf("output %q, want %q", out, want)
	}
	if rows := listRows(t, dir, "list"); len(rows) != 1 || rows["ABC002"] == nil {
		t.Errorf("after delete: %v", rows)
	}
	if _, err := runDevice(t, dir, "delete", "ABC001"); !errors.Is(err, errkind.ErrNotFound) {
		t.Errorf("second delete: %v, want not found", err)
	}
	// A MAC names a device too, in any case.
	if _, err := runDevice(t, dir, "delete", "d8:3b:da:12:34:02"); err != nil {
		t.Fatal(err)
	}
	if rows := listRows(t, dir, "list"); len(rows) != 0 {
		t.Errorf("after deleting both: %v", rows)
	}
}

func TestDeviceBlock(t *testing.T) {
	t.Parallel()
	dir := deviceDir(t)
	if out, err := runDevice(t, dir, "block", "ABC001"); err != nil || out != "Blocked D8:3B:DA:12:34:01.\n" {
		t.Fatalf("block: %q, %v", out, err)
	}
	// An unregistered MAC can be blocked too.
	if _, err := runDevice(t, dir, "block", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatal(err)
	}
	if rows := listRows(t, dir, "list"); !strings.HasSuffix(strings.Join(rows["ABC001"], " "), " yes") || strings.HasSuffix(strings.Join(rows["ABC002"], " "), " yes") {
		t.Errorf("list after block: %v", rows)
	}
	blocked := listRows(t, dir, "blocked")
	if len(blocked) != 2 || blocked["D8:3B:DA:12:34:01"][1] != "ABC001" || blocked["AA:BB:CC:DD:EE:FF"][1] != "-" {
		t.Errorf("blocked: %v", blocked)
	}
	if out, err := runDevice(t, dir, "unblock", "ABC001"); err != nil || out != "Unblocked D8:3B:DA:12:34:01.\n" {
		t.Fatalf("unblock: %q, %v", out, err)
	}
	if _, err := runDevice(t, dir, "unblock", "ABC001"); !errors.Is(err, errkind.ErrNotFound) {
		t.Errorf("second unblock: %v, want not found", err)
	}
	if _, err := runDevice(t, dir, "unblock", "AA:BB:CC:DD:EE:FF"); err != nil {
		t.Fatal(err)
	}
	if blocked := listRows(t, dir, "blocked"); len(blocked) != 0 {
		t.Errorf("after unblocking both: %v", blocked)
	}
	if _, err := runDevice(t, dir, "block", "ZZZ999"); !errors.Is(err, errkind.ErrNotFound) {
		t.Errorf("block of an unknown device: %v, want not found", err)
	}
}

func TestDeviceUsage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, args := range [][]string{{"nope"}, {"delete"}, {"block", "a", "b"}, {"blocked", "x"}} {
		if _, err := runDevice(t, dir, args...); err == nil {
			t.Errorf("device %v: no error", args)
		}
	}
}

// TestDeviceSet checks that device set changes only the settings it is
// given, clears a setting with none, and refuses a model that is not in the
// registry, a palette the model lacks, a time zone that does not load, Local,
// and a locale trmnlp lacks, each leaving the device as it was. The devices
// report no model.
func TestDeviceSet(t *testing.T) {
	t.Parallel()
	dir := deviceDir(t)
	for _, tc := range []struct {
		args    []string
		want    string
		invalid bool
	}{
		{[]string{"-name", "Kitchen", "-profile", "og_bwry", "abc001"}, "ABC001: name Kitchen, profile og_bwry, palette -, zone -, locale -.\n", false},
		{[]string{"-palette", "color-4bwry", "ABC001"}, "ABC001: name Kitchen, profile og_bwry, palette color-4bwry, zone -, locale -.\n", false},
		{[]string{"-name", "Hall", "-palette", "gray-4", "ABC001"}, "", true},
		{[]string{"-profile", "og_nope", "ABC001"}, "", true},
		{[]string{"-profile", "none", "-palette", "none", "ABC001"}, "ABC001: name Kitchen, profile -, palette -, zone -, locale -.\n", false},
		{[]string{"-name", "", "ABC001"}, "ABC001: name -, profile -, palette -, zone -, locale -.\n", false},
		{[]string{"-zone", "America/Los_Angeles", "-locale", "pt-BR", "ABC001"}, "ABC001: name -, profile -, palette -, zone America/Los_Angeles, locale pt-BR.\n", false},
		// A flag left out keeps its setting.
		{[]string{"-name", "Hall", "ABC001"}, "ABC001: name Hall, profile -, palette -, zone America/Los_Angeles, locale pt-BR.\n", false},
		{[]string{"-zone", "Local", "ABC001"}, "", true},
		{[]string{"-zone", "Nowhere/Land", "ABC001"}, "", true},
		{[]string{"-name", "Den", "-locale", "xx", "ABC001"}, "", true},
		{[]string{"-zone", "none", "ABC001"}, "ABC001: name Hall, profile -, palette -, zone -, locale pt-BR.\n", false},
		{[]string{"-locale", "none", "ABC001"}, "ABC001: name Hall, profile -, palette -, zone -, locale -.\n", false},
	} {
		out, err := runDevice(t, dir, append([]string{"set"}, tc.args...)...)
		if tc.invalid {
			if !errors.Is(err, errkind.ErrInvalid) {
				t.Errorf("%v: %q, %v; want invalid", tc.args, out, err)
			}
			continue
		}
		if err != nil || out != tc.want {
			t.Errorf("%v: %q, %v; want %q", tc.args, out, err, tc.want)
		}
	}
	if _, err := runDevice(t, dir, "set", "ABC001"); err == nil {
		t.Error("set with no setting succeeded")
	}
}
