package device

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

func TestButtonDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ stored, button, function string }{
		{"", "next", "rewind"},
		{"previous", "previous", "rewind"},
		{"identify", "next", "identify"},
		{"none", "next", "none"},
		{"send_to_me", "next", "rewind"},
		{"bogus", "next", "rewind"},
	} {
		if got := ButtonCommandOf(tc.stored); got != tc.button {
			t.Errorf("ButtonCommandOf(%q) = %q, want %q", tc.stored, got, tc.button)
		}
		if got := SpecialFunctionOf(tc.stored); got != tc.function {
			t.Errorf("SpecialFunctionOf(%q) = %q, want %q", tc.stored, got, tc.function)
		}
	}
}

// TestUpdateButtons checks that Update sets and clears the button command
// and special function, keeps them when a change leaves them out, refuses a
// value that is not in the lists, a cloud function included, with the change's
// valid fields unchanged, and that both are part of SettingsState.
func TestUpdateButtons(t *testing.T) {
	t.Parallel()
	s := func(v string) *string { return &v }
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	dev, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: testMAC, ApiKeyHash: []byte{1}, FriendlyID: "ABC123", Name: "Old", FwModel: "og", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	stored := func() (button, function, name, state string) {
		t.Helper()
		d, err := client.Read.GetDevice(ctx, dev.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d.ButtonCommand, d.SpecialFunction, d.Name, SettingsState(&d)
	}
	if b, f, _, _ := stored(); b != "" || f != "" {
		t.Fatalf("a new device has button %q and function %q, want the defaults, empty", b, f)
	}
	for _, tc := range []struct {
		name     string
		change   Change
		want     error
		button   string // The device's after the change, accepted or not.
		function string
		renamed  string
	}{
		{"set both", Change{ButtonCommand: s("previous"), SpecialFunction: s("identify")}, nil, "previous", "identify", "Old"},
		{"a change that leaves them out keeps them", Change{Name: s("New")}, nil, "previous", "identify", "New"},
		{"the same again", Change{ButtonCommand: s("previous"), SpecialFunction: s("identify")}, nil, "previous", "identify", "New"},
		{"none is a function", Change{SpecialFunction: s("none")}, nil, "previous", "none", "New"},
		{"each button command", Change{ButtonCommand: s("refresh")}, nil, "refresh", "none", "New"},
		{"bad command", Change{Name: s("Newer"), ButtonCommand: s("sideways")}, errkind.ErrInvalid, "refresh", "none", "New"},
		{"none is not a command", Change{Name: s("Newer"), ButtonCommand: s("none")}, errkind.ErrInvalid, "refresh", "none", "New"},
		{"bad function", Change{Name: s("Newer"), SpecialFunction: s("dance")}, errkind.ErrInvalid, "refresh", "none", "New"},
		{"a cloud function", Change{Name: s("Newer"), SpecialFunction: s("send_to_me")}, errkind.ErrInvalid, "refresh", "none", "New"},
		{"a bad one beside a good one", Change{ButtonCommand: s("first"), SpecialFunction: s("toggle_hold")}, errkind.ErrInvalid, "refresh", "none", "New"},
		{"clear the command", Change{ButtonCommand: s("")}, nil, "", "none", "New"},
		{"clear the function", Change{SpecialFunction: s("")}, nil, "", "", "New"},
		{"clear again", Change{ButtonCommand: s(""), SpecialFunction: s("")}, nil, "", "", "New"},
	} {
		beforeButton, beforeFunction, beforeName, beforeState := stored()
		got, err := Update(ctx, client, "abc123", tc.change)
		button, function, name, state := stored()
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
			}
			if button != beforeButton || function != beforeFunction || name != beforeName || state != beforeState {
				t.Errorf("%s: a refused change changed the device: %q, %q, %q", tc.name, button, function, name)
			}
		} else {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			if got.ButtonCommand != tc.button || got.SpecialFunction != tc.function {
				t.Errorf("%s: Update returned %q, %q", tc.name, got.ButtonCommand, got.SpecialFunction)
			}
		}
		if button != tc.button || function != tc.function || name != tc.renamed {
			t.Errorf("%s: button %q function %q name %q, want %q, %q, %q", tc.name, button, function, name, tc.button, tc.function, tc.renamed)
		}
		if tc.name == "set both" && state == beforeState {
			t.Error("the settings state did not change with the button settings")
		}
	}

	// A stored value that stopped applying does not block a change of
	// another setting, nor a form that posts it back.
	if _, err := client.Writer.ExecContext(ctx, `UPDATE device SET button_command = 'gone', special_function = 'gone' WHERE id = ?`, dev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(ctx, client, "abc123", Change{Name: s("Other"), ButtonCommand: s("gone"), SpecialFunction: s("gone")}); err != nil {
		t.Errorf("a stored value posted back as it is: %v", err)
	}
}
