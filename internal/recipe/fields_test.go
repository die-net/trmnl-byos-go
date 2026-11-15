package recipe

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

func readFieldsYAML(t *testing.T, doc string) ([]Field, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.yml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSettings(dir)
	return s.CustomFields, err
}

// TestReadFields checks the fields parsed from each type's YAML, including
// both forms of an option, a scalar that YAML types as a number or a
// boolean, a default list, and a type the server does not know.
func TestReadFields(t *testing.T) {
	t.Parallel()
	one, ten := 1.0, 10.5
	for _, tc := range []struct {
		name  string
		yaml  string
		want  Field
		input string
	}{
		{
			"string", `{keyname: a, field_type: string, name: A, description: d, help_text: h, placeholder: p, maxlength: 12, default: x, optional: true}`,
			Field{Keyname: "a", Type: "string", Label: "A", Description: "d", HelpText: "h", Placeholder: "p", MaxLength: 12, Default: Value{One: "x"}, Optional: true},
			"text",
		},
		{"url", `{keyname: a, field_type: url}`, Field{Keyname: "a", Type: "url"}, "text"},
		{"multi_string", `{keyname: a, field_type: multi_string}`, Field{Keyname: "a", Type: "multi_string"}, "text"},
		{"text", `{keyname: a, field_type: text, rows: 4}`, Field{Keyname: "a", Type: "text", Rows: 4}, "textarea"},
		{"code", `{keyname: a, field_type: code, rows: 6}`, Field{Keyname: "a", Type: "code", Rows: 6}, "textarea"},
		{
			"number", `{keyname: a, field_type: number, min: 1, max: 10.5, default: 3}`,
			Field{Keyname: "a", Type: "number", Min: &one, Max: &ten, Default: Value{One: "3"}},
			"number",
		},
		{"password", `{keyname: a, field_type: password}`, Field{Keyname: "a", Type: "password"}, "password"},
		{"boolean", `{keyname: a, field_type: boolean, default: no}`, Field{Keyname: "a", Type: "boolean", Default: Value{One: "false"}}, "boolean"},
		{"date", `{keyname: a, field_type: date, default: today}`, Field{Keyname: "a", Type: "date", Default: Value{One: "today"}}, "date"},
		{"time", `{keyname: a, field_type: time}`, Field{Keyname: "a", Type: "time"}, "time"},
		{
			"select with both option forms", `{keyname: a, field_type: select, options: ["New York", {Berlin: de}, 7, {Zürich: 8044}, ~], default: de}`,
			Field{Keyname: "a", Type: "select", Default: Value{One: "de"}, Options: []Option{
				{"New York", "new_york"}, {"Berlin", "de"}, {"7", "7"}, {"Zürich", "8044"},
			}},
			"select",
		},
		{
			"multi select with a list default", `{keyname: a, field_type: select, multiple: true, options: [X, Y], default: [x, y]}`,
			Field{Keyname: "a", Type: "select", Multiple: true, Options: []Option{{"X", "x"}, {"Y", "y"}}, Default: Value{Many: []string{"x", "y"}}},
			"select",
		},
		{"author_bio", `{keyname: a, field_type: author_bio}`, Field{Keyname: "a", Type: "author_bio"}, "none"},
		{"copyable", `{keyname: a, field_type: copyable, value: x}`, Field{Keyname: "a", Type: "copyable"}, "none"},
		{"copyable_webhook_url", `{keyname: a, field_type: copyable_webhook_url}`, Field{Keyname: "a", Type: "copyable_webhook_url"}, "none"},
		{"xhrSelect", `{keyname: a, field_type: xhrSelect, endpoint: https://x}`, Field{Keyname: "a", Type: "xhrSelect"}, "text"},
		{"xhrSelectSearch", `{keyname: a, field_type: xhrSelectSearch}`, Field{Keyname: "a", Type: "xhrSelectSearch"}, "text"},
		{"plugin_instance_select", `{keyname: a, field_type: plugin_instance_select}`, Field{Keyname: "a", Type: "plugin_instance_select"}, "text"},
		{"time_zone", `{keyname: a, field_type: time_zone}`, Field{Keyname: "a", Type: "time_zone"}, "text"},
		{"lat_lon", `{keyname: a, field_type: lat_lon}`, Field{Keyname: "a", Type: "lat_lon"}, "text"},
		{"unknown type", `{keyname: a, field_type: hologram, conditional_validation: [{when: x}]}`, Field{Keyname: "a", Type: "hologram", HasConditions: true}, "text"},
		{"numeric keyname and no type", `{keyname: 7}`, Field{Keyname: "7"}, "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := readFieldsYAML(t, "name: x\ncustom_fields:\n- "+tc.yaml+"\n")
			if err != nil || len(got) != 1 {
				t.Fatalf("fields = %+v, %v", got, err)
			}
			if !reflect.DeepEqual(got[0], tc.want) {
				t.Errorf("field = %+v\nwant    %+v", got[0], tc.want)
			}
			if got[0].Input() != tc.input {
				t.Errorf("Input() = %q, want %q", got[0].Input(), tc.input)
			}
		})
	}
}

func TestReadFieldsRefused(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"not a list":          "custom_fields: 7",
		"a map":               "custom_fields: {keyname: a}",
		"entry not a map":     "custom_fields: [city]",
		"no keyname":          "custom_fields: [{field_type: string}]",
		"duplicate keyname":   "custom_fields: [{keyname: a}, {keyname: b}, {keyname: a}]",
		"option is a list":    "custom_fields: [{keyname: a, options: [[x]]}]",
		"maxlength not a n":   "custom_fields: [{keyname: a, maxlength: many}]",
		"optional not a b":    "custom_fields: [{keyname: a, optional: maybe}]",
		"default holds a map": "custom_fields: [{keyname: a, default: [{x: y}]}]",
	} {
		_, err := readFieldsYAML(t, doc)
		if !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%s: err = %v, want errkind.ErrInvalid", name, err)
		}
	}
}

func TestFieldRequiredAndSecret(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		f                Field
		required, secret bool
	}{
		{"plain", Field{Type: "string"}, true, false},
		{"optional", Field{Type: "string", Optional: true}, false, false},
		{"default", Field{Type: "string", Default: Value{One: "x"}}, false, false},
		{"an empty list default is a default, as in trmnlp", Field{Type: "select", Default: Value{Many: []string{}}}, false, false},
		{"blank default", Field{Type: "string", Default: Value{One: "  "}}, true, false},
		{"boolean", Field{Type: "boolean"}, false, false},
		{"conditional validation", Field{Type: "string", HasConditions: true}, false, false},
		{"select with a blank option", Field{Type: "select", Options: []Option{{"None", ""}, {"A", "a"}}}, false, false},
		{"select with a blank-looking option", Field{Type: "select", Options: []Option{{"None", " "}}}, false, false},
		{"select with no blank option", Field{Type: "select", Options: []Option{{"A", "a"}}}, true, false},
		{"password", Field{Type: "password"}, true, true},
		{"display only", Field{Type: "author_bio"}, false, false},
	} {
		if got := tc.f.Required(); got != tc.required {
			t.Errorf("%s: Required() = %v, want %v", tc.name, got, tc.required)
		}
		if got := tc.f.Secret(); got != tc.secret {
			t.Errorf("%s: Secret() = %v, want %v", tc.name, got, tc.secret)
		}
	}
}

func TestValueJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		json string
		want Value
		ok   bool
	}{
		{`"x"`, Value{One: "x"}, true},
		{`""`, Value{}, true},
		{`["a","b"]`, Value{Many: []string{"a", "b"}}, true},
		{`[]`, Value{Many: []string{}}, true},
		{`null`, Value{}, false},
		{`7`, Value{}, false},
		{`true`, Value{}, false},
		{`[1]`, Value{}, false},
		{`["a",null]`, Value{}, false},
		{`[null]`, Value{}, false},
		{`{"a":"b"}`, Value{}, false},
	} {
		var got Value
		err := json.Unmarshal([]byte(tc.json), &got)
		if (err == nil) != tc.ok || (!tc.ok && !errors.Is(err, errkind.ErrInvalid)) {
			t.Errorf("%s: err = %v, want ok = %v and, if not, errkind.ErrInvalid", tc.json, err, tc.ok)
			continue
		}
		if tc.ok {
			if !got.equal(tc.want) {
				t.Errorf("%s: got %+v, want %+v", tc.json, got, tc.want)
			}
			if out, err := json.Marshal(got); err != nil || string(out) != tc.json {
				t.Errorf("%s: marshals to %s, %v", tc.json, out, err)
			}
		}
	}
	// In a map of values, as the API and the database hold them. A null is
	// refused there too: encoding/json calls UnmarshalJSON for it.
	var m map[string]Value
	if err := json.Unmarshal([]byte(`{"a":"x","b":["y"],"c":[]}`), &m); err != nil || m["a"].One != "x" || !m["b"].IsList() || !m["c"].IsList() || !m["c"].Empty() {
		t.Errorf("a map of values: %v, %v", m, err)
	}
	if err := json.Unmarshal([]byte(`{"a":"x","c":null}`), &m); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("a map of values with a null: %v, want ErrInvalid", err)
	}
	// The YAML decoder is the one the recipe's settings.yml goes through.
	var f Field
	if err := yaml.Unmarshal([]byte(`keyname: a`), &f); err != nil || f.Keyname != "a" {
		t.Errorf("Field from YAML: %+v, %v", f, err)
	}
}

// TestReadFieldsPsych checks that settings.yml is read as Ruby's Psych reads
// it for booleans, which trmnlp does: a plain yes, no, true, false, on or
// off in any case is a boolean, and so "true" or "false" as text, in flags,
// options and defaults. A quoted one, and y and n, stay as written.
func TestReadFieldsPsych(t *testing.T) {
	t.Parallel()
	got, err := readFieldsYAML(t, `name: x
custom_fields:
- {keyname: a, field_type: string, optional: yes, multiple: On}
- {keyname: b, field_type: select, options: [Yes, nO, "yes", y, N, OFF, New York, {Ja: Yes}], default: [Yes, "yes"]}
- {keyname: c, field_type: string, default: TRUE}
- {keyname: d, field_type: select, options: [{Berlin: de}, "New York"], default: Berlin}
- {keyname: e, field_type: select, multiple: true, options: [{Berlin: de}, "New York", X], default: [Berlin, new_york, x, X]}
`)
	if err != nil || len(got) != 5 {
		t.Fatalf("%+v, %v", got, err)
	}
	if !got[0].Optional || !got[0].Multiple {
		t.Errorf("optional: yes, multiple: On: %+v", got[0])
	}
	labels, values := make([]string, 0, len(got[1].Options)), make([]string, 0, len(got[1].Options))
	for _, o := range got[1].Options {
		labels, values = append(labels, o.Label), append(values, o.Value)
	}
	if strings.Join(labels, ",") != "true,false,yes,y,N,false,New York,Ja" || strings.Join(values, ",") != "true,false,yes,y,n,false,new_york,true" {
		t.Errorf("options: labels %v, values %v", labels, values)
	}
	if !got[1].Default.equal(Value{Many: []string{"true", "yes"}}) || got[2].Default.One != "true" {
		t.Errorf("defaults %+v, %+v", got[1].Default, got[2].Default)
	}
	// A default given as a label is stored as the option's value, per
	// element for a list, and one that is a value or matches nothing is kept.
	if got[3].Default.One != "de" || !got[4].Default.equal(Value{Many: []string{"de", "new_york", "x", "x"}}) {
		t.Errorf("label defaults %+v, %+v", got[3].Default, got[4].Default)
	}
}

// TestReadSettingsAliases checks that an alias is refused, with its line,
// before it can make the decoder repeat the work of a field's unmarshaler,
// and that a recipe without aliases reads as before. The bomb is small, and
// would take minutes and gigabytes if it were decoded.
func TestReadSettingsAliases(t *testing.T) {
	t.Parallel()
	var bomb strings.Builder
	bomb.WriteString("name: x\ncustom_fields:\n- &f\n  keyname: a\n  field_type: select\n  options: [" + strings.Repeat("opt, ", 2000) + "z]\n")
	for range 5000 {
		bomb.WriteString("- *f\n")
	}
	start := time.Now()
	_, err := readFieldsYAML(t, bomb.String())
	if !errors.Is(err, errkind.ErrInvalid) || !strings.Contains(err.Error(), "line 7") {
		t.Errorf("err = %v, want errkind.ErrInvalid at line 7", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the refusal took %v", d)
	}
	for name, doc := range map[string]string{
		"merge key":     "name: x\nbase: &b {keyname: a}\ncustom_fields:\n- <<: *b\n",
		"in a scalar":   "name: x\nrefresh_interval: *r\n",
		"in the fields": "custom_fields:\n- &a {keyname: a}\n- *a\n",
	} {
		if _, err := readFieldsYAML(t, doc); !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// An anchor that nothing uses is only a label.
	got, err := readFieldsYAML(t, "name: x\ncustom_fields:\n- &a {keyname: a}\n")
	if err != nil || len(got) != 1 {
		t.Errorf("an unused anchor: %+v, %v", got, err)
	}
}

// TestSettingsTooBig checks that a settings.yml over MaxSettingsBytes is
// refused as invalid, by a read and at install, however it is built, and
// that one at the cap is read.
func TestSettingsTooBig(t *testing.T) {
	t.Parallel()
	head := "name: x\ncustom_fields:\n- keyname: a\n  field_type: select\n  options: ["
	pad := func(n int) string { return head + strings.Repeat("a,", (n-len(head))/2) + "z]\n" }
	if _, err := readFieldsYAML(t, pad(MaxSettingsBytes-8)); err != nil {
		t.Errorf("a file at the cap: %v", err)
	}
	for name, doc := range map[string]string{
		"a long option list": pad(300 << 10),
		"a junk key":         "name: x\njunk: [" + strings.Repeat("a,", 150<<10) + "z]\n",
		"comments":           "name: x\n" + strings.Repeat("# padding\n", 40000),
	} {
		_, err := readFieldsYAML(t, doc)
		if !errors.Is(err, errkind.ErrInvalid) || !strings.Contains(err.Error(), "over 256 KB") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// A recipe with such a file is not installed.
	src := t.TempDir()
	writeDir(t, src, map[string]string{"settings.yml": pad(300 << 10), "full.liquid": "x"})
	recipes := filepath.Join(t.TempDir(), "recipes")
	client := openDB(t)
	if _, err := Install(t.Context(), client, recipes, src, time.Now()); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("install: %v", err)
	}
	zipPath := filepath.Join(t.TempDir(), "r.zip")
	writeZip(t, zipPath, map[string]string{"settings.yml": pad(300 << 10), "full.liquid": "x"})
	if _, err := Install(t.Context(), client, recipes, zipPath, time.Now()); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("install of a zip: %v", err)
	}
	if rows, err := client.Read.ListRecipes(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("recipes recorded: %v, %v", rows, err)
	}
}
