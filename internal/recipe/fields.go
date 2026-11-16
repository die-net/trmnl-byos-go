package recipe

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// Option is one choice of a select field.
type Option struct {
	// Label is what a form shows.
	Label string
	// Value is what an instance stores and the recipe sees.
	Value string
}

// Field is one entry of a recipe's custom_fields: a setting an instance
// of the recipe has a value for.
type Field struct {
	// Keyname is the name the recipe's Liquid reads the value by.
	Keyname string
	// Label is what a form calls the field: the YAML key name.
	Label string
	// Type is the field_type as the recipe wrote it.
	Type        string
	Description string
	HelpText    string
	Placeholder string
	// Default is what trmnlp uses for a blank value. It is the zero Value
	// if there is none.
	Default Value
	// Optional is whether the field may stay blank.
	Optional bool
	// Options are a select field's choices.
	Options []Option
	// Multiple is whether a select field takes several of its options.
	Multiple bool
	// Min and Max bound a number field, and are nil when unset.
	Min, Max *float64
	// MaxLength is the most characters a text-like field may hold, 0 for no
	// limit.
	MaxLength int
	// Rows is how many lines a text or code field shows.
	Rows int
	// HasConditions is whether the field has a conditional_validation. Its
	// rules are ignored, but trmnlp counts a blank value for such a field as
	// an answer, so it is never required.
	HasConditions bool
}

// Secret reports whether the field is a password field, whose value is
// always stored sealed and never shown back. An instance may also have other
// fields marked secret by the admin: see CanMarkSecret.
func (f *Field) Secret() bool { return f.Type == "password" }

// CanMarkSecret reports whether an admin may mark the field secret for an
// instance, so its value is stored sealed and never shown back as a password
// field's is. Many recipes declare a credential as a string or text field,
// so the recipe's type alone cannot be trusted to say what is secret. Only a
// field that takes free text can be marked: the choice of a select, a number,
// a boolean, a date or a time is not a credential. A password field is
// always secret, so it cannot be marked or unmarked.
func (f *Field) CanMarkSecret() bool {
	in := f.Input()
	return in == "text" || in == "textarea"
}

// secretWords are words that, alone in a field's keyname or label, suggest
// it holds a credential.
var secretWords = []string{
	"auth", "authorization", "bearer", "credential", "credentials", "passphrase",
	"passwd", "password", "pwd", "secret", "token",
}

// secretKeyWords are the words that, just before "key" in a field's keyname
// or label, suggest it holds a credential, as "api key" does. "key" alone
// does not, since "sort key" does not.
var secretKeyWords = []string{
	"access", "account", "api", "app", "client", "consumer", "developer",
	"license", "private", "secret", "subscription",
}

// secretGlued are run-together words that suggest a credential when they
// are part of one word of a keyname or label, as in "openaiapikey".
var secretGlued = []string{
	"accesstoken", "apikey", "apitoken", "authtoken", "bearertoken",
	"clientsecret", "passwd", "password", "privatekey", "secretkey",
}

// LooksSecret reports whether the field's keyname or label looks like a
// credential, such as api_key, apiKey, "Access Token" or client_secret. A
// password field always does. It is only a guess a form uses to tick its
// secret box in advance for a field that CanMarkSecret: nothing is stored
// sealed unless the admin leaves the box ticked.
func (f *Field) LooksSecret() bool {
	if f.Secret() {
		return true
	}
	if !f.CanMarkSecret() {
		return false
	}
	return looksSecret(words(f.Keyname)) || looksSecret(words(f.Label))
}

// looksSecret reports whether a field name's words suggest a credential.
func looksSecret(ws []string) bool {
	for i, w := range ws {
		if slices.Contains(secretWords, w) {
			return true
		}
		if i > 0 && w == "key" && slices.Contains(secretKeyWords, ws[i-1]) {
			return true
		}
		// An access ID, such as AWS's access_id, comes with a secret.
		if i > 0 && w == "id" && ws[i-1] == "access" {
			return true
		}
		if slices.ContainsFunc(secretGlued, func(g string) bool { return strings.Contains(w, g) }) {
			return true
		}
	}
	return false
}

// words splits a keyname or label into lowercase words, at every character
// that is not a letter or digit, where a lowercase letter or digit is
// followed by an uppercase one, as in apiKey, and before the last capital of
// a run that a lowercase letter follows, as in AWSAccessKey.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		var prev, next rune
		if i > 0 {
			prev = rs[i-1]
		}
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
			continue
		case unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev)):
			flush()
		case unicode.IsUpper(r) && unicode.IsUpper(prev) && unicode.IsLower(next):
			flush()
		}
		cur = append(cur, unicode.ToLower(r))
	}
	flush()
	return out
}

// Input is what a form shows for the field and how its value is checked:
// "text", "textarea", "number", "select", "boolean", "date", "time" or
// "password", or "none" for a type that only displays something and takes
// no value. A type the server does not know is "text".
func (f *Field) Input() string {
	switch f.Type {
	case "text", "code":
		return "textarea"
	case "number", "select", "boolean", "date", "time", "password":
		return f.Type
	case "author_bio", "copyable", "copyable_webhook_url":
		return "none"
	}
	return "text"
}

// Required reports whether an instance must have a value for the field: it
// is not optional, takes a value at all, and blank is not an answer, as
// trmnlp's blank_is_a_valid_answer? has it. Blank is an answer for a
// boolean, a field with a default, a field with a conditional_validation,
// and a select with an option whose value is blank.
func (f *Field) Required() bool {
	return !f.Optional && f.Input() != "none" && !f.blankIsAnswer()
}

// blankIsAnswer reports whether a blank value is a valid answer for the
// field.
func (f *Field) blankIsAnswer() bool {
	hasDefault := f.Default.IsList() || strings.TrimSpace(f.Default.One) != ""
	return f.Input() == "boolean" || hasDefault || f.HasConditions ||
		slices.ContainsFunc(f.Options, func(o Option) bool { return strings.TrimSpace(o.Value) == "" })
}

// Value is a field's value: one string, or a list of strings for a
// multi-select.
type Value struct {
	// One is the value of a field that takes one.
	One string
	// Many is the value of a multi-select. A non-nil Many is a list, even
	// an empty one, and then One is not used.
	Many []string
}

// IsList reports whether the value is a list.
func (v Value) IsList() bool { return v.Many != nil }

// Empty reports whether the value is blank: a string with nothing but white
// space in it, or a list with no elements. trmnlp takes such a value as none
// at all. An empty value is how a field is cleared. The value is stored as it
// was given when it is not empty.
func (v Value) Empty() bool {
	if v.IsList() {
		return len(v.Many) == 0
	}
	return strings.TrimSpace(v.One) == ""
}

// same reports whether v and o are the same value. Two empty values are
// the same, since an empty value is not stored.
func (v Value) same(o Value) bool {
	if v.Empty() && o.Empty() {
		return true
	}
	return v.IsList() == o.IsList() && v.One == o.One && slices.Equal(v.Many, o.Many)
}

// equal reports whether the two values are the same string or the same
// list.
func (v Value) equal(o Value) bool {
	return v.One == o.One && v.IsList() == o.IsList() && slices.Equal(v.Many, o.Many)
}

// MarshalJSON encodes the value as a JSON string, or as an array of strings
// for a list.
func (v Value) MarshalJSON() ([]byte, error) {
	if v.IsList() {
		return json.Marshal(v.Many)
	}
	return json.Marshal(v.One)
}

// UnmarshalJSON decodes a JSON string or an array of strings. Anything else
// is an error that wraps errkind.ErrInvalid.
func (v *Value) UnmarshalJSON(b []byte) error {
	// encoding/json hands over a value with no space around it. A null
	// decodes into a string or a slice without error, so the first byte
	// decides, and a null is refused like any other kind.
	refused := fmt.Errorf("%w: a field value is a string or an array of strings", errkind.ErrInvalid)
	switch {
	case len(b) > 0 && b[0] == '"':
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return refused
		}
		*v = Value{One: one}
	case len(b) > 0 && b[0] == '[':
		// A null element decodes into a string as "", so the elements are
		// pointers, and a nil one is refused.
		var items []*string
		if err := json.Unmarshal(b, &items); err != nil {
			return refused
		}
		many := make([]string, len(items))
		for i, item := range items {
			if item == nil {
				return refused
			}
			many[i] = *item
		}
		*v = Value{Many: many}
	default:
		return refused
	}
	return nil
}

// UnmarshalYAML decodes one entry of custom_fields. It takes each scalar's
// own text as its string, so 7, true and no are "7", "true" and "no", as
// the same text is a recipe's select value or default. An entry that is not
// a map, or that has no keyname, is an error that wraps errkind.ErrInvalid.
func (f *Field) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("%w: line %d: a custom field is not a map", errkind.ErrInvalid, node.Line)
	}
	attrs := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		attrs[node.Content[i].Value] = node.Content[i+1]
	}
	p := fieldParser{attrs: attrs}
	*f = Field{
		Keyname:     p.text("keyname"),
		Label:       p.text("name"),
		Type:        p.text("field_type"),
		Description: p.text("description"),
		HelpText:    p.text("help_text"),
		Placeholder: p.text("placeholder"),
		Default:     p.value("default"),
		Optional:    p.bool("optional"),
		Options:     p.options("options"),
		Multiple:    p.bool("multiple"),
		Min:         p.float("min"),
		Max:         p.float("max"),
		MaxLength:   p.int("maxlength"),
		Rows:        p.int("rows"),
	}
	if c := attrs["conditional_validation"]; c != nil && c.Tag != "!!null" {
		f.HasConditions = true
	}
	if f.Input() == "select" {
		f.Default = f.stored(f.Default)
	}
	if p.err == nil && f.Keyname == "" {
		p.err = fmt.Errorf("line %d: no keyname", node.Line)
	}
	if p.err != nil {
		return fmt.Errorf("%w: custom field %q: %w", errkind.ErrInvalid, f.Keyname, p.err)
	}
	return nil
}

// fieldParser reads the attributes of one custom field. It keeps the first
// error and returns zero values after it, so the caller checks once.
type fieldParser struct {
	attrs map[string]*yaml.Node
	err   error
}

// fail records err for the attribute key, unless an error is recorded
// already.
func (p *fieldParser) fail(key string, err error) {
	if p.err == nil {
		p.err = fmt.Errorf("%s: %w", key, err)
	}
}

// scalar returns the text of the scalar attribute key, and whether it is
// set. A missing or null attribute is not set. Anything but a scalar is an
// error.
func (p *fieldParser) scalar(key string) (string, bool) {
	n := p.attrs[key]
	if n == nil || n.Tag == "!!null" {
		return "", false
	}
	if n.Kind != yaml.ScalarNode {
		p.fail(key, fmt.Errorf("line %d: not a single value", n.Line))
		return "", false
	}
	return n.Value, true
}

func (p *fieldParser) text(key string) string {
	s, _ := p.scalar(key)
	return s
}

func (p *fieldParser) bool(key string) bool {
	if _, ok := p.scalar(key); !ok {
		return false
	}
	s := plainText(p.attrs[key])
	b, err := strconv.ParseBool(s)
	if err != nil {
		p.fail(key, fmt.Errorf("%q is not true or false", s))
	}
	return b
}

func (p *fieldParser) int(key string) int {
	s, ok := p.scalar(key)
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		p.fail(key, fmt.Errorf("%q is not a whole number", s))
	}
	return n
}

func (p *fieldParser) float(key string) *float64 {
	s, ok := p.scalar(key)
	if !ok {
		return nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		p.fail(key, fmt.Errorf("%q is not a number", s))
		return nil
	}
	return &n
}

// value reads a scalar, or a list of scalars for a multi-select.
func (p *fieldParser) value(key string) Value {
	n := p.attrs[key]
	if n == nil || n.Kind != yaml.SequenceNode {
		if _, ok := p.scalar(key); !ok {
			return Value{}
		}
		return Value{One: plainText(n)}
	}
	many := make([]string, 0, len(n.Content))
	for _, item := range n.Content {
		if item.Kind != yaml.ScalarNode {
			p.fail(key, fmt.Errorf("line %d: a list holds only single values", item.Line))
			break
		}
		many = append(many, plainText(item))
	}
	return Value{Many: many}
}

// options reads a select field's choices. A plain scalar is its own label,
// and its value as trmnlp stores it: in lower case with spaces as
// underscores. A one-pair map is label: value. A null entry is skipped, as
// trmnlp does.
func (p *fieldParser) options(key string) []Option {
	n := p.attrs[key]
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	opts := make([]Option, 0, len(n.Content))
	for _, item := range n.Content {
		switch {
		case item.Kind == yaml.ScalarNode && item.Tag == "!!null":
		case item.Kind == yaml.ScalarNode:
			label := plainText(item)
			opts = append(opts, Option{Label: label, Value: strings.ReplaceAll(strings.ToLower(label), " ", "_")})
		case item.Kind == yaml.MappingNode && len(item.Content) >= 2 && item.Content[0].Kind == yaml.ScalarNode && item.Content[1].Kind == yaml.ScalarNode:
			opts = append(opts, Option{Label: plainText(item.Content[0]), Value: plainText(item.Content[1])})
		default:
			p.fail(key, fmt.Errorf("line %d: an option is a value or a label: value pair", item.Line))
		}
	}
	return opts
}

// plainText returns the text of a scalar node as Ruby's Psych, which
// trmnlp and TRMNL parse settings.yml with, would turn it into a string. Psych
// reads a plain yes, true or on in any case as a boolean, and no, false or
// off, so they are "true" and "false" there; yaml.v3 follows YAML 1.2 and
// leaves some as text. A quoted or tagged scalar, and any other text, is
// left alone. The single letters y and n are strings in Psych, so they are
// left alone too.
func plainText(n *yaml.Node) string {
	const notPlain = yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle | yaml.TaggedStyle
	if n.Style&notPlain == 0 {
		switch strings.ToLower(n.Value) {
		case "yes", "true", "on":
			return "true"
		case "no", "false", "off":
			return "false"
		}
	}
	return n.Value
}

// stored returns v as trmnlp's stored_value has it for a select: a label
// becomes its option's value, element by element for a list, and anything
// that is a value already, or no option's label, is kept.
func (f *Field) stored(v Value) Value {
	one := func(s string) string {
		if s == "" || slices.ContainsFunc(f.Options, func(o Option) bool { return o.Value == s }) {
			return s
		}
		if i := slices.IndexFunc(f.Options, func(o Option) bool { return o.Label == s }); i >= 0 {
			return f.Options[i].Value
		}
		return s
	}
	if !v.IsList() {
		return Value{One: one(v.One)}
	}
	many := make([]string, len(v.Many))
	for i, s := range v.Many {
		many[i] = one(s)
	}
	return Value{Many: many}
}

// checkFields refuses a recipe whose custom fields repeat a keyname, since
// a value could not say which field it is for.
func checkFields(fields []Field) error {
	seen := make(map[string]bool, len(fields))
	for i := range fields {
		if seen[fields[i].Keyname] {
			return fmt.Errorf("%w: custom field %q is defined twice", errkind.ErrInvalid, fields[i].Keyname)
		}
		seen[fields[i].Keyname] = true
	}
	return nil
}
