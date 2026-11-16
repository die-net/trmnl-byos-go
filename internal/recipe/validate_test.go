package recipe

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

func TestFieldErrors(t *testing.T) {
	t.Parallel()
	err := FieldErrors{"b": "is bad", "a": "is worse"}
	if got, want := err.Error(), "a: is worse; b: is bad"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, errkind.ErrInvalid) {
		t.Error("FieldErrors is not errkind.ErrInvalid")
	}
}

// checkPasswords runs checkValues with only the password fields sealed, and
// returns the values, or a FieldErrors if any are refused.
func checkPasswords(fields []Field, values map[string]Value, creating bool) (map[string]Value, error) {
	byName := fieldsByName(fields)
	bad := FieldErrors{}
	out := checkValues(fields, values, creating, func(k string) bool { return byName[k] != nil && byName[k].Secret() }, bad)
	if len(bad) > 0 {
		return nil, bad
	}
	return out, nil
}

// TestCheckValues checks each rule of validation, with the message for the
// field that breaks it.
func TestCheckValues(t *testing.T) {
	t.Parallel()
	one, ten := 1.0, 10.0
	fields := []Field{
		{Keyname: "str", Type: "string", MaxLength: 4, Optional: true},
		{Keyname: "area", Type: "text", MaxLength: 3, Optional: true},
		{Keyname: "sealed", Type: "password", MaxLength: 8, Optional: true},
		{Keyname: "num", Type: "number", Min: &one, Max: &ten, Optional: true},
		{Keyname: "free", Type: "number", Optional: true},
		{Keyname: "sel", Type: "select", Options: []Option{{"A", "a"}, {"B", "b"}}, Optional: true},
		{Keyname: "multi", Type: "select", Multiple: true, Options: []Option{{"A", "a"}, {"B", "b"}}, Optional: true},
		{Keyname: "bool", Type: "boolean", Optional: true},
		{Keyname: "date", Type: "date", Optional: true},
		{Keyname: "time", Type: "time", Optional: true},
		{Keyname: "zone", Type: "time_zone", Optional: true},
		{Keyname: "bio", Type: "author_bio"},
		{Keyname: "must", Type: "string"},
		{Keyname: "dflt", Type: "string", Default: Value{One: "x"}},
		{Keyname: "flag", Type: "boolean"},
		{Keyname: "cond", Type: "string", HasConditions: true},
		{Keyname: "pick", Type: "select", Options: []Option{{"None", ""}, {"A", "a"}}},
	}
	must := Value{One: "ok"}
	for _, tc := range []struct {
		name     string
		values   map[string]Value
		creating bool
		want     map[string]string // field errors by keyname, nil for none
	}{
		{"nothing to check", nil, false, nil},
		{"unknown field", map[string]Value{"nope": {One: "x"}}, false, map[string]string{"nope": "no such field"}},
		{"unknown and empty on update, which UpdateInstance checks against what is stored", map[string]Value{"nope": {}}, false, nil},
		{"unknown and empty on create", map[string]Value{"nope": {}, "must": {One: "x"}}, true, map[string]string{"nope": "no such field"}},
		{"display only with a value", map[string]Value{"bio": {One: "x"}}, false, map[string]string{"bio": "takes no value"}},
		{"list for a string", map[string]Value{"str": {Many: []string{"a"}}}, false, map[string]string{"str": "takes one value, not a list"}},
		{"list for a single select", map[string]Value{"sel": {Many: []string{"a"}}}, false, map[string]string{"sel": "takes one value, not a list"}},
		{
			"too long",
			map[string]Value{"str": {One: "abcde"}, "area": {One: "éééé"}},
			false,
			map[string]string{"str": "must be at most 4 characters", "area": "must be at most 3 characters"},
		},
		{"long enough in characters", map[string]Value{"str": {One: "éééé"}}, false, nil},
		{"short password", map[string]Value{"sealed": {One: "abcde"}}, false, map[string]string{"sealed": "must be at least 6 characters to be secret"}},
		{"password counted in characters", map[string]Value{"sealed": {One: "éééééé"}}, false, nil},
		{"long password", map[string]Value{"sealed": {One: "abcdefghi"}}, false, map[string]string{"sealed": "must be at most 8 characters"}},
		{"number ok", map[string]Value{"num": {One: "2.5"}, "free": {One: "-1e3"}}, false, nil},
		{"number at bounds", map[string]Value{"num": {One: "10"}}, false, nil},
		{"number below", map[string]Value{"num": {One: "0.5"}}, false, map[string]string{"num": "must be at least 1"}},
		{"number above", map[string]Value{"num": {One: "10.5"}}, false, map[string]string{"num": "must be at most 10"}},
		{"not a number", map[string]Value{"free": {One: "ten"}}, false, map[string]string{"free": "must be a number"}},
		{"NaN is not a number", map[string]Value{"free": {One: "NaN"}}, false, map[string]string{"free": "must be a number"}},
		{"infinity is not a number", map[string]Value{"free": {One: "Inf"}}, false, map[string]string{"free": "must be a number"}},
		{"select ok", map[string]Value{"sel": {One: "a"}}, false, nil},
		{"select by label", map[string]Value{"sel": {One: "A"}}, false, map[string]string{"sel": "is not one of the options"}},
		{"multi ok", map[string]Value{"multi": {Many: []string{"a", "b"}}}, false, nil},
		{"multi one bad", map[string]Value{"multi": {Many: []string{"a", "c"}}}, false, map[string]string{"multi": "is not one of the options"}},
		{"multi with a choice twice", map[string]Value{"multi": {Many: []string{"a", "b", "a"}}}, false, nil},
		{"multi given a string", map[string]Value{"multi": {One: "b"}}, false, nil},
		{"bool ok", map[string]Value{"bool": {One: "false"}}, false, nil},
		{"bool bad", map[string]Value{"bool": {One: "yes"}}, false, map[string]string{"bool": `must be "true" or "false"`}},
		{"date ok", map[string]Value{"date": {One: "2026-02-28"}}, false, nil},
		{"date bad", map[string]Value{"date": {One: "2026-02-30"}}, false, map[string]string{"date": "must be a date as YYYY-MM-DD"}},
		{"date in another form", map[string]Value{"date": {One: "28/02/2026"}}, false, map[string]string{"date": "must be a date as YYYY-MM-DD"}},
		{"time ok", map[string]Value{"time": {One: "23:59"}}, false, nil},
		{"time midnight", map[string]Value{"time": {One: "00:00"}}, false, nil},
		{"time 24", map[string]Value{"time": {One: "24:00"}}, false, map[string]string{"time": "must be a time as HH:MM, from 00:00 to 23:59"}},
		{"time with a short hour", map[string]Value{"time": {One: "9:05"}}, false, map[string]string{"time": "must be a time as HH:MM, from 00:00 to 23:59"}},
		{"time with seconds", map[string]Value{"time": {One: "09:05:00"}}, false, map[string]string{"time": "must be a time as HH:MM, from 00:00 to 23:59"}},
		{"text type is not checked", map[string]Value{"zone": {One: "Mars/Olympus"}}, false, nil},
		{"required missing on create", nil, true, map[string]string{"must": "is required"}},
		{"required empty on create", map[string]Value{"must": {}}, true, map[string]string{"must": "is required"}},
		{"white space is not a value on create", map[string]Value{"must": {One: " \t "}}, true, map[string]string{"must": "is required"}},
		{"white space clears, so cannot clear a required field", map[string]Value{"must": {One: "  "}}, false, map[string]string{"must": "is required"}},
		{"white space clears an optional field", map[string]Value{"str": {One: "  "}}, false, nil},
		{"NUL in a string", map[string]Value{"str": {One: "a\x00b"}}, false, map[string]string{"str": "contains a NUL byte"}},
		{"NUL in a password", map[string]Value{"sealed": {One: "abcdef\x00"}}, false, map[string]string{"sealed": "contains a NUL byte"}},
		{"NUL in a list", map[string]Value{"multi": {Many: []string{"a", "\x00"}}}, false, map[string]string{"multi": "contains a NUL byte"}},
		{"required given on create", map[string]Value{"must": must}, true, nil},
		{"required not checked when left out of an update", map[string]Value{"str": {One: "a"}}, false, nil},
		{"required cleared by an update", map[string]Value{"must": {Many: []string{}}}, false, map[string]string{"must": "is required"}},
		{"blank is an answer for a boolean, a conditional field and a select with a blank option", map[string]Value{"must": must}, true, nil},
		{"and each can be cleared", map[string]Value{"flag": {}, "cond": {}, "pick": {}}, false, nil},
		{"a default makes it optional", map[string]Value{"dflt": {}}, false, nil},
		{
			"several at once",
			map[string]Value{"num": {One: "x"}, "bool": {One: "x"}, "nope": {One: "x"}},
			true,
			map[string]string{"num": "must be a number", "bool": `must be "true" or "false"`, "nope": "no such field", "must": "is required"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := checkPasswords(fields, tc.values, tc.creating)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			var fe FieldErrors
			if !errors.As(err, &fe) || !errors.Is(err, errkind.ErrInvalid) || got != nil {
				t.Fatalf("got %v, err = %v; want a FieldErrors", got, err)
			}
			if len(fe) != len(tc.want) {
				t.Errorf("errors = %v, want %v", fe, tc.want)
			}
			for k, msg := range tc.want {
				if fe[k] != msg {
					t.Errorf("%s: %q, want %q", k, fe[k], msg)
				}
			}
		})
	}
}

// TestCheckValuesNormalizes checks that a single string for a multi-select
// comes back as a one-element list and that a clearing value is kept, so a
// caller can tell it from a key that was left out.
func TestCheckValuesNormalizes(t *testing.T) {
	t.Parallel()
	fields := []Field{
		{Keyname: "multi", Type: "select", Multiple: true, Optional: true, Options: []Option{{"A", "a"}}},
		{Keyname: "s", Type: "string", Optional: true},
	}
	got, err := checkPasswords(fields, map[string]Value{"multi": {One: "a"}, "s": {}}, false)
	if err != nil || len(got) != 2 || !got["multi"].IsList() || !slices.Equal(got["multi"].Many, []string{"a"}) || !got["s"].Empty() {
		t.Errorf("got %v, %v", got, err)
	}
	if _, err := checkPasswords(fields, map[string]Value{"multi": {One: "z"}}, false); err == nil || !strings.Contains(err.Error(), "multi: ") {
		t.Errorf("a bad single choice: %v", err)
	}
}

// TestOptionsSharingAValue checks that a multi-select whose options share a
// value, as Red and red do, or Metric: m and Meters: m, stores the value
// once, in the order of its first option, also when the list names it
// twice, as a form does that ticks both options.
func TestOptionsSharingAValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		options []Option
		chosen  []string
		want    []string
	}{
		{"labels that lowercase alike", []Option{{"Blue", "blue"}, {"Red", "red"}, {"red", "red"}}, []string{"red", "blue"}, []string{"blue", "red"}},
		{"two labels with one value", []Option{{"Metric", "m"}, {"Meters", "m"}, {"Imperial", "i"}}, []string{"i", "m"}, []string{"m", "i"}},
	} {
		fields := []Field{{Keyname: "multi", Type: "select", Multiple: true, Optional: true, Options: tc.options}}
		got, err := checkPasswords(fields, map[string]Value{"multi": {Many: tc.chosen}}, false)
		if err != nil || !slices.Equal(got["multi"].Many, tc.want) {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got["multi"], err, tc.want)
		}
		twice := []string{tc.chosen[0], tc.chosen[0]}
		got, err = checkPasswords(fields, map[string]Value{"multi": {Many: twice}}, false)
		if err != nil || !slices.Equal(got["multi"].Many, twice[:1]) {
			t.Errorf("%s: a value chosen twice: got %v, %v; want %v", tc.name, got["multi"], err, twice[:1])
		}
	}
}

// TestStoredForm checks that StoredForm gives a value as the field stores
// it, and a value the field refuses, or an empty one, as it is.
func TestStoredForm(t *testing.T) {
	t.Parallel()
	multi := Field{Keyname: "colors", Type: "select", Multiple: true, Options: []Option{{"Red", "red"}, {"red", "red"}, {"Green", "green"}}}
	notes := Field{Keyname: "notes", Type: "text"}
	for _, tc := range []struct {
		name  string
		f     Field
		v     Value
		wantV Value
	}{
		{"a shared value posted twice", multi, Value{Many: []string{"red", "red"}}, Value{Many: []string{"red"}}},
		{"choices out of order", multi, Value{Many: []string{"green", "red"}}, Value{Many: []string{"red", "green"}}},
		{"no choice", multi, Value{Many: []string{}}, Value{Many: []string{}}},
		{"a choice that is not an option is kept", multi, Value{Many: []string{"blue", "red"}}, Value{Many: []string{"blue", "red"}}},
		{"CR LF in a textarea", notes, Value{One: "a\r\nb"}, Value{One: "a\nb"}},
	} {
		got := tc.f.StoredForm(tc.v)
		if got.One != tc.wantV.One || !slices.Equal(got.Many, tc.wantV.Many) || got.IsList() != tc.wantV.IsList() {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.wantV)
		}
	}
}

// TestMessagesDoNotEchoValues checks that no message of a bad value repeats
// the value, since one may be a secret put in the wrong field.
func TestMessagesDoNotEchoValues(t *testing.T) {
	t.Parallel()
	one := 1.0
	fields := []Field{
		{Keyname: "sel", Type: "select", Options: []Option{{"A", "a"}}},
		{Keyname: "multi", Type: "select", Multiple: true, Options: []Option{{"A", "a"}}},
		{Keyname: "num", Type: "number", Min: &one},
		{Keyname: "bool", Type: "boolean"},
		{Keyname: "date", Type: "date"},
		{Keyname: "time", Type: "time"},
		{Keyname: "str", Type: "string", MaxLength: 3},
		{Keyname: "pw", Type: "password"},
		{Keyname: "bio", Type: "author_bio"},
	}
	const secret = "hunter2-secret\x00"
	for _, f := range fields {
		for _, v := range []Value{{One: secret}, {Many: []string{secret, secret}}} {
			_, err := checkPasswords(fields, map[string]Value{f.Keyname: v}, false)
			if err == nil || strings.Contains(err.Error(), "hunter2") {
				t.Errorf("%s with %v: %v", f.Keyname, v, err)
			}
		}
	}
}
