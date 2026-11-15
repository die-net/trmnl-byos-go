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
	// hiddenPrefix names the field a form sends for each value input it
	// showed empty because the field was secret, so a blank from it is
	// known not to be a value the admin emptied.
	hiddenPrefix = "h."
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
	// HiddenName is the one sent when the value is not shown.
	Name, SecretName, ClearName, HiddenName string
	Label                                   string
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
		ID: "field-" + strconv.Itoa(i), Name: valuePrefix + k, SecretName: secretPrefix + k, ClearName: clearPrefix + k, HiddenName: hiddenPrefix + k,
		Label: cmp.Or(f.Label, k), Input: in, Required: f.Required(), Placeholder: f.Placeholder, MaxLength: f.MaxLength,
		Rows: min(cmp.Or(f.Rows, 4), 20), Multiple: in == "select" && f.Multiple,
	}
	for _, h := range []string{f.Description, f.HelpText} {
		if strings.TrimSpace(h) != "" {
			v.Hints = append(v.Hints, h)
		}
	}
	msg, bad := src.errs[k]
	if bad {
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
	// sealed is whether a sealed value is stored, and plain whether a plain
	// one is. A plain value is not shown while its field is secret: a
	// password field that was another type when it was stored, or a field
	// whose box is ticked. Then a blank input keeps it, as it keeps a
	// sealed one, so a page that hides a value never clears it.
	sealed := src.editing && slices.Contains(src.secretNames, k)
	plain := src.editing && !src.stored[k].Empty()
	// The Remove box is for a stored value the form does not show. One it
	// shows is cleared by emptying its input.
	v.CanClear = (sealed || plain && v.Secret) && !f.Required()
	v.ClearChecked = v.CanClear && src.posted.Has(v.ClearName)
	if bad && msg == recipe.SealedUnmarkText {
		// The domain's words name a clear, which this form calls the
		// Remove box, and has only when the field is not required.
		if v.CanClear {
			v.Error = v.Label + " is secret. Give it a new value, or tick Remove the stored value, to make it plain."
		} else {
			v.Error = v.Label + " is secret. Give it a new value to make it plain."
		}
	}

	switch {
	case !src.editing:
	case v.Secret && (sealed || plain):
		v.Hints = append(v.Hints, "A value is stored. Leave this empty to keep it.")
	case v.Secret && src.secretNames == nil:
		v.Hints = append(v.Hints, "Leave this empty to keep a stored value, if there is one.")
	case sealed && v.CanClear:
		v.Hints = append(v.Hints, "A secret value is stored and cannot be shown. Give a new value, or remove the stored one, to make this field plain.")
	case sealed:
		v.Hints = append(v.Hints, "A secret value is stored and cannot be shown. Give a new value to make this field plain.")
	case v.Secret:
		v.Hints = append(v.Hints, "No value is stored.")
	}
	// A typed secret is not put back in the page. A posted value that is
	// the stored plain one was only hidden by the tick, and is kept.
	if v.Secret && src.posted != nil {
		if typed := postedValue(f, src.posted); !typed.Empty() && (!plain || !sameValue(f, typed, src.stored[k])) {
			v.Hints = append(v.Hints, "Enter the secret value again. It is not shown back.")
		}
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
		// A sealed value cannot be compared or shown, so it gets no note.
		if src.conflict && !sealed && !sameValue(f, val, src.stored[k]) {
			v.Hints = append(v.Hints, differsText, storedNow(storedText(f, src.stored[k])))
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
		v := postedValue(f, src.posted)
		if v.Empty() && src.posted.Has(hiddenPrefix+f.Keyname) {
			// The page that was posted hid the value, so its blank is not
			// an emptied input, and the stored value is shown.
			return src.stored[f.Keyname]
		}
		return v
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

// maxStoredShown is how many characters of a stored value a note shows.
const maxStoredShown = 200

// storedNow is the note that says what is stored now, after differsText.
// text is "" when nothing is stored.
func storedNow(text string) string {
	if text == "" {
		return "Nothing is stored now."
	}
	if r := []rune(text); len(r) > maxStoredShown {
		return "Stored now, cut short: " + string(r[:maxStoredShown])
	}
	return "Stored now: " + text
}

// storedText returns a stored plain value as a note shows it: an option's
// label for a select, several joined with commas, and on or off for a
// boolean.
func storedText(f *recipe.Field, v recipe.Value) string {
	if f.Input() == "boolean" {
		if v.One == "true" {
			return "on"
		}
		return "off"
	}
	items := v.Many
	if !v.IsList() && v.One != "" {
		items = []string{v.One}
	}
	if f.Input() != "select" {
		return strings.Join(items, ", ")
	}
	labels := make([]string, len(items))
	for i, it := range items {
		labels[i] = it
		if j := slices.IndexFunc(f.Options, func(o recipe.Option) bool { return o.Value == it }); j >= 0 {
			labels[i] = f.Options[j].Label
		}
	}
	return strings.Join(labels, ", ")
}

// instanceUpdate returns the change a posted form makes to an instance that
// has the stored plain values and the marks. It holds only what differs
// from what is stored, so a save that changes no value never has to open
// the stored secrets. A blank input means keep, and is left out, when the
// field is secret in the form, by its type or its ticked box, or when the
// page that was posted did not show the value, whether the stored value is
// sealed or plain: a blank the page made is not a request to clear. A value
// is cleared by emptying an input that showed it, with the box unticked, or
// with the Remove box.
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
			// The input may still hold the stored value it showed, which is
			// no new value. A new value given with a clear is refused by
			// the domain, which says so.
			if !val.Empty() && !sameValue(f, val, stored[k]) {
				u.Values[k] = val
			}
		case f.Secret() || ticked || post.Has(hiddenPrefix+k):
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
