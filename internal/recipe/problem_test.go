package recipe

import (
	"path/filepath"
	"testing"
)

// TestInterpolatedWithoutDefault checks the match of a keyname in a polling
// URL against trmnlp's, whose word boundary is Ruby's and knows Unicode
// letters: a longer name is another field, a dot or a dash ends the name,
// and a default filter, even right after the name, makes the use one that
// needs no value.
func TestInterpolatedWithoutDefault(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		url, keyname string
		want         bool
	}{
		{"{{ city }}", "city", true},
		{"{{city}}", "city", true},
		{"{{- city -}}", "city", true},
		{"{{ city_name }}", "city", false},
		{"{{ citywide }}", "city", false},
		{"{{ city.name }}", "city", true},
		{"{{ api-key }}", "api", true},
		{"{{ city | default: 'x' }}", "city", false},
		{"{{ city| default: 'x' }}", "city", false},
		{"{{ city | upcase | default: 'x' }}", "city", false},
		{"{{ city | default: 'x' }}{{ city }}", "city", true},
		{"{{ trmnl.plugin_settings.custom_fields_values.city }}", "city", true},
		{"{% if city %}", "city", false},
		{"{{ café }}", "café", true},
		{"{{ cafée }}", "café", false},
		{"{{ café2 }}", "café", false},
	} {
		if got := interpolatedWithoutDefault(tc.url, tc.keyname); got != tc.want {
			t.Errorf("%q in %q: %v, want %v", tc.keyname, tc.url, got, tc.want)
		}
	}
}

// TestBuildProblem checks what fails a build that trmnlp would only warn
// about: a strategy this server does not run, and, for a polling recipe, a
// required field with no value that the polling URL interpolates without a
// default filter, which is trmnlp's own rule. A field that is optional, or
// whose blank is an answer, may be empty, a recipe with no polling URL is
// not checked, and a strategy that is not a scalar names none, so its
// recipe still reads.
func TestBuildProblem(t *testing.T) {
	t.Parallel()
	const fields = `custom_fields:
- keyname: api_key
  field_type: string
- keyname: unit
  field_type: string
- keyname: units
  field_type: select
  options: [metric, imperial]
  default: metric
- keyname: note
  field_type: string
  optional: true
`
	const keyURL = "polling_url: https://api.example/x?key={{ api_key }}\n"
	set := map[string]Value{"api_key": {One: "k"}}
	for _, tc := range []struct {
		name, yml string
		values    map[string]Value
		want      string
	}{
		{"polling, all set", "strategy: polling\n" + keyURL + fields, set, ""},
		{"polling, used field missing", "strategy: polling\n" + keyURL + fields, nil, "required fields have no value: api_key"},
		{"polling, used field blank", "strategy: polling\n" + keyURL + fields, map[string]Value{"api_key": {One: " "}}, "required fields have no value: api_key"},
		{"polling, the long name", "strategy: polling\npolling_url: https://api.example/x?key={{- trmnl.plugin_settings.custom_fields_values.api_key }}\n" + fields, nil, "required fields have no value: api_key"},
		{"polling, a required field the url does not use", "strategy: polling\n" + keyURL + fields, set, ""},
		{"polling, only with a default", "strategy: polling\npolling_url: \"https://api.example/x?key={{ api_key }}&u={{ unit | default: 'm' }}\"\n" + fields, set, ""},
		{"polling, once with and once without a default", "strategy: polling\npolling_url: \"https://api.example/x?u={{ unit | default: 'm' }}&v={{ unit }}&key={{ api_key }}\"\n" + fields, set, "required fields have no value: unit"},
		{"polling, a longer keyname is not this one", "strategy: polling\npolling_url: https://api.example/x?k={{ api_key_2 }}\n" + fields, nil, ""},
		{"polling, no url", "strategy: polling\n" + fields, nil, ""},
		{"polling, empty url", "strategy: polling\npolling_url: \"  \"\n" + fields, nil, ""},
		{"polling, url as a list", "strategy: polling\npolling_url:\n- https://api.example/a\n- https://api.example/b?key={{ api_key }}\n" + fields, nil, "required fields have no value: api_key"},
		{"webhook", "strategy: webhook\n" + keyURL + fields, nil, ""},
		{"async polling", "strategy: async_polling\n" + keyURL + fields, set, "the recipe gets its data by async polling, which this server does not do"},
		{"plugin merge", "strategy: plugin_merge\n" + fields, nil, "the recipe merges another plugin's data, which this server does not do"},
		{"strategy as a list", "strategy: [polling]\n" + keyURL + fields, nil, ""},
		{"strategy as a map", "strategy: {a: b}\n" + keyURL + fields, nil, ""},
	} {
		dir := t.TempDir()
		writeDir(t, dir, map[string]string{"settings.yml": "name: R\n" + tc.yml})
		s, err := ReadSettings(filepath.Clean(dir))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := s.BuildProblem(tc.values); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
