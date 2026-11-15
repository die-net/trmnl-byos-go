package recipe

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// parse decodes JSON the way a push does, or fails the test.
func parse(t *testing.T, s string) Data {
	t.Helper()
	d, err := ParseData(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseData(t *testing.T) {
	t.Parallel()
	for _, s := range []string{`[1]`, `"x"`, `null`, ``, `{"a":1} {"b":2}`, `{"a":`} {
		if _, err := ParseData(strings.NewReader(s)); !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%q: error %v, want errkind.ErrInvalid", s, err)
		}
	}
}

func TestDeepMerge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, base, add, want string }{
		{"empty base", `{}`, `{"a":1}`, `{"a":1}`},
		{"adds and replaces", `{"a":1,"b":2}`, `{"b":3,"c":4}`, `{"a":1,"b":3,"c":4}`},
		{"nested objects merge", `{"o":{"x":1,"y":{"p":1}}}`, `{"o":{"y":{"q":2},"z":3}}`, `{"o":{"x":1,"y":{"p":1,"q":2},"z":3}}`},
		{"arrays are replaced", `{"l":[1,2,3]}`, `{"l":[9]}`, `{"l":[9]}`},
		{"object over scalar", `{"o":1}`, `{"o":{"x":1}}`, `{"o":{"x":1}}`},
		{"scalar over object", `{"o":{"x":1}}`, `{"o":2}`, `{"o":2}`},
		{"null replaces", `{"a":{"x":1}}`, `{"a":null}`, `{"a":null}`},
	} {
		got := DeepMerge(parse(t, tc.base), parse(t, tc.add))
		if want := parse(t, tc.want); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, want)
		}
	}
	if got := DeepMerge(nil, parse(t, `{"a":1}`)); len(got) != 1 {
		t.Errorf("nil base: %v", got)
	}
}

func TestStream(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, base, add string
		limit           int
		want            string
	}{
		{"appends", `{"l":[1,2]}`, `{"l":[3]}`, 0, `{"l":[1,2,3]}`},
		{"new key", `{}`, `{"l":[1]}`, 0, `{"l":[1]}`},
		{"drops the oldest", `{"l":[1,2,3]}`, `{"l":[4,5]}`, 4, `{"l":[2,3,4,5]}`},
		{"limit under the push", `{"l":[1]}`, `{"l":[2,3,4]}`, 2, `{"l":[3,4]}`},
		{"under the limit", `{"l":[1]}`, `{"l":[2]}`, 5, `{"l":[1,2]}`},
		{"other keys are replaced", `{"a":1,"l":[1]}`, `{"a":2,"s":"x"}`, 3, `{"a":2,"l":[1],"s":"x"}`},
		{"array over scalar", `{"l":1}`, `{"l":[2]}`, 0, `{"l":[2]}`},
		{"scalar over array", `{"l":[1]}`, `{"l":2}`, 0, `{"l":2}`},
		{"objects in an array stay whole", `{"l":[{"a":1}]}`, `{"l":[{"a":2}]}`, 0, `{"l":[{"a":1},{"a":2}]}`},
	} {
		got := Stream(parse(t, tc.base), parse(t, tc.add), tc.limit)
		if want := parse(t, tc.want); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, want)
		}
	}
}

// TestProjectData checks that pushed data goes in the project's variables
// at the top level, next to trmnl.device, as typed YAML: a string that
// looks like Liquid or has a newline stays a literal string, and a number
// stays a number.
func TestProjectData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDir(t, dir, map[string]string{"settings.yml": settingsYML, "full.liquid": "x"})
	shared := NewProject(dir, nil, "UTC", nil)
	data := `{"title": "{{ env.FIELD_0 }} {% raw %}", "body": "a\nb: c", "n": 3, "big": 12345678901234567890, "f": 1.5,
		"list": [{"k": "v"}, 7], "t": true, "none": null, "trmnl": {"device": {"friendly_id": "evil"}}}`

	p, err := shared.WithData(data)
	if err != nil {
		t.Fatal(err)
	}
	p = p.WithDevice(Device{FriendlyID: "ABC123"})
	got := projectConfig(t, p)
	for _, want := range []string{
		"    title: '{{ env.FIELD_0 }} {% raw %}'\n",
		"    body: |-\n        a\n        b: c\n",
		"    \"n\": 3\n",
		"    big: 12345678901234567890\n",
		"    f: 1.5\n",
		"    list:\n        - k: v\n        - 7\n",
		"    t: true\n",
		"    none: null\n",
		"    trmnl:\n        device:\n            friendly_id: ABC123\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf(".trmnlp.yml lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "evil") {
		t.Errorf("pushed trmnl key reached the project:\n%s", got)
	}
	// The order does not matter, and the shared project is not changed.
	q, err := shared.WithDevice(Device{FriendlyID: "ABC123"}).WithData(`{"n": 3}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := projectConfig(t, q); !strings.Contains(got, "    \"n\": 3\n") || !strings.Contains(got, "friendly_id: ABC123") {
		t.Errorf("device then data:\n%s", got)
	}
	if strings.Contains(projectConfig(t, shared), "variables") {
		t.Error("WithData changed the project it was called on")
	}
	// A project that already has data is not changed either.
	before := projectConfig(t, p)
	r, err := p.WithData(`{"other": 1}`)
	if err != nil {
		t.Fatal(err)
	}
	if after := projectConfig(t, p); after != before || strings.Contains(after, "other") {
		t.Errorf("WithData changed the project it was called on:\n%s", after)
	}
	if got := projectConfig(t, r); !strings.Contains(got, "other: 1") || !strings.Contains(got, "friendly_id: ABC123") {
		t.Errorf("WithData result:\n%s", got)
	}
	if _, err := shared.WithData(`[1]`); err == nil {
		t.Error("WithData took an array")
	}
}

func TestPushData(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := openDB(t)
	rec, err := client.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "r", Source: "s", RefreshInterval: 900, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := client.Write.CreateInstance(ctx, db.CreateInstanceParams{RecipeID: rec.ID, Name: "i", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	push := func(strategy, add string, limit int) ([]byte, error) {
		return PushData(ctx, client, PushParams{InstanceID: inst.ID, Strategy: strategy, Add: parse(t, add), StreamLimit: limit}, now)
	}
	if out, err := push("", `{"l":[1]}`, 0); err != nil || string(out) != `{"l":[1]}` {
		t.Fatalf("replace: %s, %v", out, err)
	}
	if out, err := push(StrategyStream, `{"l":[2]}`, 0); err != nil || string(out) != `{"l":[1,2]}` {
		t.Fatalf("stream: %s, %v", out, err)
	}
	row, err := client.Read.GetInstanceData(ctx, inst.ID)
	if err != nil || row.Data != `{"l":[1,2]}` || row.UpdatedAt != 101 {
		t.Fatalf("stored %+v, %v", row, err)
	}
	// The first push was stored at 100, then the second at 101. A push in
	// the same second, or at an earlier time, is stored one second after the last, so a build that read the last can tell it is newer.
	for _, at := range []int64{100, 50, 101} {
		now = time.Unix(at, 0)
		if _, err := push(StrategyDeepMerge, `{}`, 0); err != nil {
			t.Fatal(err)
		}
	}
	if row, _ := client.Read.GetInstanceData(ctx, inst.ID); row.UpdatedAt != 104 {
		t.Errorf("updated_at %d after pushes at 100, 50 and 101, want 104", row.UpdatedAt)
	}
	// A refused push keeps what was there and its time.
	now = time.Unix(200, 0)
	if _, err := push("bogus", `{}`, 0); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("unknown strategy: %v", err)
	}
	if _, err := push(StrategyDeepMerge, `{"big":"`+strings.Repeat("x", MaxDataBytes)+`"}`, 0); !errors.Is(err, ErrDataTooBig) {
		t.Errorf("big result: %v", err)
	}
	if _, err := PushData(ctx, client, PushParams{InstanceID: 99, Add: Data{}}, now); !errors.Is(err, ErrNoInstance) {
		t.Errorf("unknown instance: %v", err)
	}
	if row, _ := client.Read.GetInstanceData(ctx, inst.ID); row.Data != `{"l":[1,2]}` || row.UpdatedAt != 104 {
		t.Errorf("a refused push changed %+v", row)
	}
}
