package adminui

import (
	"cmp"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/die-net/trmnl-byos-go/internal/recipe"
)

// Form field name prefixes. A recipe's keyname is appended, so a keyname can
// never collide with the name and palette fields.
const (
	valuePrefix  = "f."
	secretPrefix = "s."
	clearPrefix  = "c."
)

// optionView is one choice of a select field.
type optionView struct {
	// ID is the element ID of a checkbox of a multi-select.
	ID       string
	Label    string
	Value    string
	Selected bool
}

// fieldView is one custom field of a recipe as a form shows it. Element IDs
// come from the field's position, never from its keyname, which a recipe
// controls.
type fieldView struct {
	// ID is the element ID of the control, such as "field-3".
	ID string
	// Name, SecretName and ClearName are the form field names of the value,
	// of the box that marks the value secret, and of the one that clears it.
	Name, SecretName, ClearName string
	Label                       string
	// Input is the field's Input: "text", "textarea", "number", "select",
	// "boolean", "date", "time", "password" or "none".
	Input string
	// Hints are the paragraphs under the control, tied to it with
	// aria-describedby.
	Hints []string
	// Described is the value of aria-describedby, or "".
	Described string
	// Required is whether the field is marked Required, and HTMLRequired
	// whether the control has the required attribute, which is left off where
	// it could block a save the domain allows.
	Required, HTMLRequired bool
	Placeholder            string
	MaxLength, Rows        int
	// Min and Max bound a number, or are "".
	Min, Max string
	// Value is the text of a single-value control. It is "" for a secret
	// field, which is never shown.
	Value string
	// Options are a select's choices, and Multiple says it takes several.
	// BlankOption is whether a single select starts with an empty choice.
	Options     []optionView
	Multiple    bool
	BlankOption bool
	// Checked is whether a boolean is on.
	Checked bool
	// Secret is whether the field's value is secret now, so it is not shown.
	Secret bool
	// CanMark is whether the admin may mark the field secret, and Marked
	// whether the box is ticked.
	CanMark, Marked bool
	// CanClear is whether the stored secret value can be cleared, and
	// ClearChecked whether the box is ticked.
	CanClear, ClearChecked bool
	// Error is the sentence that says why the field was refused, or "".
	Error string
}

// fieldSource is where a form gets what it shows: the posted form of a
// refused post, or else what is stored or the defaults.
type fieldSource struct {
	// posted is the refused form, nil for the first render of a page.
	posted url.Values
	// editing is whether the form edits an instance, not creates one.
	editing bool
	// stored are an instance's plain values, for an edit.
	stored map[string]recipe.Value
	// secretNames are the keynames of the secret fields that have a value,
	// nil when they could not be read, and marks the keynames marked secret.
	secretNames, marks []string
	// errs says what was wrong with each field of a refused post.
	errs recipe.FieldErrors
	// conflict is whether the post was refused because the instance changed
	// after its page was sent. Each field whose posted value is not what is
	// stored now then says so, since saving again replaces the stored value.
	conflict bool
}

// differsText is the note on a field of a refused stale save whose value on
// the page is not the one stored now.
const differsText = "This is not what is stored now. Saving again replaces the stored value with this one."

// build returns a view of each field, and a sentence for each error that
// belongs to no field shown. It also returns how many errors are shown in
// a field.
func (src *fieldSource) build(fields []recipe.Field) (views []fieldView, orphans []string, shown int) {
	rendered := make(map[string]bool, len(fields))
	views = make([]fieldView, len(fields))
	for i := range fields {
		views[i] = src.view(i, &fields[i])
		if views[i].Input != "none" {
			rendered[fields[i].Keyname] = true
		}
		if views[i].Error != "" {
			shown++
		}
	}
	for _, k := range slices.Sorted(maps.Keys(src.errs)) {
		if !rendered[k] {
			orphans = append(orphans, k+": "+src.errs[k]+".")
		}
	}
	return views, orphans, shown
}

// view returns the view of field i.
func (src *fieldSource) view(i int, f *recipe.Field) fieldView {
	k := f.Keyname
	in := f.Input()
	v := fieldView{
		ID: "field-" + strconv.Itoa(i), Name: valuePrefix + k, SecretName: secretPrefix + k, ClearName: clearPrefix + k,
		Label: cmp.Or(f.Label, k), Input: in, Required: f.Required(), Placeholder: f.Placeholder, MaxLength: f.MaxLength,
		Rows: min(cmp.Or(f.Rows, 4), 20), Multiple: in == "select" && f.Multiple,
	}
	for _, h := range []string{f.Description, f.HelpText} {
		if strings.TrimSpace(h) != "" {
			v.Hints = append(v.Hints, h)
		}
	}
	if msg, ok := src.errs[k]; ok {
		v.Error = v.Label + " " + msg + "."
	}
	if in == "none" {
		v.Described = describedBy(&v)
		return v
	}
	if f.Min != nil {
		v.Min = strconv.FormatFloat(*f.Min, 'g', -1, 64)
	}
	if f.Max != nil {
		v.Max = strconv.FormatFloat(*f.Max, 'g', -1, 64)
	}

	v.CanMark = f.CanMarkSecret()
	if v.CanMark {
		switch {
		case src.posted != nil:
			v.Marked = src.posted.Has(v.SecretName)
		case src.editing:
			v.Marked = slices.Contains(src.marks, k)
		default:
			v.Marked = f.LooksSecret()
		}
	}
	v.Secret = f.Secret() || v.Marked
	// A password field's value is plain if it was stored before the field
	// became one. It is not shown either, and can be removed like a sealed one.
	stored := src.editing && (slices.Contains(src.secretNames, k) || f.Secret() && !src.stored[k].Empty())
	v.CanClear = stored && !f.Required()
	v.ClearChecked = v.CanClear && src.posted.Has(v.ClearName)

	switch {
	case !src.editing:
	case v.Secret && src.secretNames == nil:
		v.Hints = append(v.Hints, "Leave this empty to keep a stored value, if there is one.")
	case v.Secret && stored:
		v.Hints = append(v.Hints, "A value is stored. Leave this empty to keep it.")
	case stored && v.CanClear:
		v.Hints = append(v.Hints, "A secret value is stored and cannot be shown. Give a new value, or remove the stored one, to make this field plain.")
	case stored:
		v.Hints = append(v.Hints, "A secret value is stored and cannot be shown. Give a new value to make this field plain.")
	case v.Secret:
		v.Hints = append(v.Hints, "No value is stored.")
	}
	if v.Secret && src.posted != nil && src.posted.Get(v.Name) != "" {
		v.Hints = append(v.Hints, "Enter the secret value again. It is not shown back.")
	}
	if v.Secret && !src.editing && in == "password" {
		v.Hints = append(v.Hints, "It is stored encrypted and never shown again.")
	}

	// An update checks only the values it changes, so the edit form never
	// uses the required attribute, which would refuse a rename over a field
	// the recipe made required later.
	v.HTMLRequired = v.Required && !src.editing && !v.Multiple && in != "boolean"

	if !v.Secret {
		val := src.value(f)
		fillControl(&v, f, val)
		if src.conflict && !sameValue(f, val, src.stored[k]) {
			v.Hints = append(v.Hints, differsText)
		}
	}
	v.Described = describedBy(&v)
	return v
}

// fill sets the control's value, choices and checked state from val.
func fillControl(v *fieldView, f *recipe.Field, val recipe.Value) {
	switch v.Input {
	case "boolean":
		v.Checked = val.One == "true"
	case "select":
		chosen := val.Many
		if !val.IsList() && (v.Multiple && val.One != "" || !v.Multiple) {
			chosen = []string{val.One}
		}
		v.Options = make([]optionView, 0, len(f.Options)+1)
		for j, o := range f.Options {
			v.Options = append(v.Options, optionView{ID: v.ID + "-o" + strconv.Itoa(j), Label: o.Label, Value: o.Value, Selected: slices.Contains(chosen, o.Value)})
		}
		// A value the recipe's options no longer have is kept chosen, so a
		// save does not change it to the first option unseen.
		for _, c := range chosen {
			if c != "" && !slices.ContainsFunc(f.Options, func(o recipe.Option) bool { return o.Value == c }) {
				v.Options = append(v.Options, optionView{ID: v.ID + "-o" + strconv.Itoa(len(v.Options)), Label: c, Value: c, Selected: true})
			}
		}
		v.BlankOption = !v.Multiple && !f.Required() && !slices.ContainsFunc(f.Options, func(o recipe.Option) bool { return strings.TrimSpace(o.Value) == "" })
	default:
		if !val.IsList() {
			v.Value = val.One
		}
	}
}

// value returns what the form shows for the field: what was posted, or what
// the instance has, or the field's default.
func (src *fieldSource) value(f *recipe.Field) recipe.Value {
	switch {
	case src.posted != nil:
		return postedValue(f, src.posted)
	case src.editing:
		return src.stored[f.Keyname]
	}
	return f.Default
}

// postedValue returns the value a form posted for the field. An unchecked
// box posts nothing, so a boolean is "true" when its box was ticked and
// "false" when it was not.
func postedValue(f *recipe.Field, post url.Values) recipe.Value {
	name := valuePrefix + f.Keyname
	switch {
	case f.Input() == "boolean":
		return recipe.Value{One: strconv.FormatBool(post.Get(name) == "true")}
	case f.Input() == "select" && f.Multiple:
		// A list even when no box was ticked, which clears the field.
		return recipe.Value{Many: append([]string{}, post[name]...)}
	}
	// A browser sends each line break of a form value as CR LF. A value is
	// stored with the line breaks it was typed with, which are LF.
	return recipe.Value{One: strings.ReplaceAll(post.Get(name), "\r\n", "\n")}
}

// describedBy returns the IDs of the hints and the error of v.
func describedBy(v *fieldView) string {
	ids := make([]string, 0, len(v.Hints)+1)
	for i := range v.Hints {
		ids = append(ids, fmt.Sprintf("%s-hint%d", v.ID, i))
	}
	if v.Error != "" {
		ids = append(ids, v.ID+"-err")
	}
	return strings.Join(ids, " ")
}

// postedValues returns the values a form posted for every field that takes
// one.
func postedValues(fields []recipe.Field, post url.Values) map[string]recipe.Value {
	out := make(map[string]recipe.Value, len(fields))
	for i := range fields {
		if fields[i].Input() != "none" {
			out[fields[i].Keyname] = postedValue(&fields[i], post)
		}
	}
	return out
}

// sameValue reports whether a posted value is what an instance has stored.
// Two blank values are the same, and a boolean that was never set is off.
func sameValue(f *recipe.Field, posted, stored recipe.Value) bool {
	if f.Input() == "boolean" && stored.Empty() {
		stored = recipe.Value{One: "false"}
	}
	if posted.Empty() && stored.Empty() {
		return true
	}
	return posted.IsList() == stored.IsList() && posted.One == stored.One && slices.Equal(posted.Many, stored.Many)
}

// markedSecret returns the secret marks a form posted, by keyname, for every
// field that can be marked.
func markedSecret(fields []recipe.Field, post url.Values) map[string]bool {
	out := map[string]bool{}
	for i := range fields {
		if fields[i].CanMarkSecret() {
			out[fields[i].Keyname] = post.Has(secretPrefix + fields[i].Keyname)
		}
	}
	return out
}

// instanceUpdate returns the change a posted form makes to an instance that
// has the stored plain values and the marks. It holds only what differs
// from what is stored, so a save that changes no value never has to open
// the stored secrets, and a blank secret input, which means keep, is left
// out.
func instanceUpdate(fields []recipe.Field, post url.Values, stored map[string]recipe.Value, marks []string, name, palette string) recipe.InstanceUpdate {
	u := recipe.InstanceUpdate{Name: &name, Palette: &palette, Values: map[string]recipe.Value{}, Secret: map[string]bool{}}
	for i := range fields {
		f := &fields[i]
		if f.Input() == "none" {
			continue
		}
		k := f.Keyname
		val := postedValue(f, post)
		ticked := f.CanMarkSecret() && post.Has(secretPrefix+k)
		if f.CanMarkSecret() && ticked != slices.Contains(marks, k) {
			u.Secret[k] = ticked
		}
		cleared := post.Has(clearPrefix + k)
		if cleared {
			u.Clear = append(u.Clear, k)
		}
		switch {
		case cleared:
			// A value given with a clear is refused by the domain, which says so.
			if !val.Empty() {
				u.Values[k] = val
			}
		case ticked && val.Empty() && !stored[k].Empty():
			// The page showed the plain value and it was emptied, so it is
			// cleared, not sealed as it was.
			u.Clear = append(u.Clear, k)
		case f.Secret() || ticked:
			// A blank means keep.
			if !val.Empty() {
				u.Values[k] = val
			}
		case !sameValue(f, val, stored[k]):
			u.Values[k] = val
		}
	}
	return u
}
