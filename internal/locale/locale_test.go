package locale

import (
	"slices"
	"testing"
)

func TestValid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		code string
		want bool
	}{
		{"en", true},
		{"de-AT", true},
		{"pt-BR", true},
		{"zh-HK", true},
		{"", false},
		{"xx", false},
		{"EN", false},
		{"pt-br", false},
		{"raw", false},
		{"en-US", false},
		{" en", false},
	} {
		if got := Valid(tc.code); got != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.code, got, tc.want)
		}
	}
	if !Valid(Default) {
		t.Errorf("the default %q is not a locale", Default)
	}
}

func TestCodes(t *testing.T) {
	t.Parallel()
	got := Codes()
	if len(got) != 28 {
		t.Errorf("%d locales, want the 28 of trmnlp v0.34.0", len(got))
	}
	if sorted := slices.Sorted(slices.Values(got)); len(slices.Compact(sorted)) != len(got) {
		t.Errorf("a locale is listed twice: %v", got)
	}
	// The slice is a copy, so a caller cannot change the list.
	got[0] = "xx"
	if Valid("xx") || Codes()[0] == "xx" {
		t.Error("changing the returned slice changed the list")
	}
}
