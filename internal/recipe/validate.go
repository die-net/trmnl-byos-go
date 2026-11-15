package recipe

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// FieldErrors says what is wrong with each field of a request, by keyname,
// so a form can mark every bad field at once.
type FieldErrors map[string]string

// Error lists the problems sorted by keyname, as "keyname: message; ...".
func (e FieldErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, k := range slices.Sorted(maps.Keys(e)) {
		parts = append(parts, k+": "+e[k])
	}
	return strings.Join(parts, "; ")
}

// Unwrap returns errkind.ErrInvalid, so a caller answers it as bad input.
func (FieldErrors) Unwrap() error { return errkind.ErrInvalid }

// MinSecretLen is the fewest characters a secret field's value may have,
// whether it is a password field or one the admin marked secret. A shorter
// value is easy to guess, so sealing it protects nothing, and redacting it
// from a build log would hide every place its characters appear. Such a
// value can be a plain field.
const MinSecretLen = 6

// shortSecret returns why s is too short to be a secret field's value, ""
// if it is not.
func shortSecret(s string) string {
	if utf8.RuneCountInString(s) < MinSecretLen {
		return fmt.Sprintf("must be at least %d characters to be secret", MinSecretLen)
	}
	return ""
}

// checkValues checks values against the recipe's field definitions and
// returns them with a single string for a multi-select turned into a list.
// sealed says whether a keyname's value is stored sealed, which it then has
// to be long enough for. Every problem is added to bad, so one FieldErrors
// reports them all. A value that is empty clears its field, so only its
// being required is checked, and on update it may name a field the recipe
// no longer has, so a value left from one can be cleared; UpdateInstance
// refuses it if no such value is stored. When creating, each required field
// also needs a value, given or not.
func checkValues(fields []Field, values map[string]Value, creating bool, sealed func(string) bool, bad FieldErrors) map[string]Value {
	byName := fieldsByName(fields)
	out := make(map[string]Value, len(values))
	for k, v := range values {
		f := byName[k]
		switch {
		case f == nil && (creating || !v.Empty()):
			bad[k] = "no such field"
		case v.Empty():
			out[k] = v
		default:
			var msg string
			if out[k], msg = f.check(v, sealed(k)); msg != "" {
				bad[k] = msg
			}
		}
	}
	for i := range fields {
		f := &fields[i]
		v, given := out[f.Keyname]
		if f.Required() && bad[f.Keyname] == "" && (given || creating) && v.Empty() {
			bad[f.Keyname] = "is required"
		}
	}
	return out
}

// fieldsByName indexes fields by keyname.
func fieldsByName(fields []Field) map[string]*Field {
	byName := make(map[string]*Field, len(fields))
	for i := range fields {
		byName[fields[i].Keyname] = &fields[i]
	}
	return byName
}

// check returns the value, a single string for a multi-select as a list,
// and why it is not acceptable for the field, "" if it is. v is not empty,
// and sealed says whether it is stored sealed. A message never repeats the
// value, which may be a secret put in the wrong field.
func (f *Field) check(v Value, sealed bool) (Value, string) {
	input := f.Input()
	if input == "none" {
		return v, "takes no value"
	}
	multi := input == "select" && f.Multiple
	if v.IsList() && !multi {
		return v, "takes one value, not a list"
	}
	if multi && !v.IsList() {
		v = Value{Many: []string{v.One}}
	}
	// A NUL byte cannot pass through the environment a build gets values in.
	if slices.ContainsFunc(v.items(), func(s string) bool { return strings.ContainsRune(s, 0) }) {
		return v, "contains a NUL byte"
	}
	switch input {
	case "select":
		for i, s := range v.items() {
			if slices.Contains(v.items()[:i], s) {
				return v, "chooses an option twice"
			}
			if !slices.ContainsFunc(f.Options, func(o Option) bool { return o.Value == s }) {
				return v, "is not one of the options"
			}
		}
		if multi {
			// A form lists the choices in option order, so they are stored
			// in it, and a save that changes nothing stores the same list.
			v = Value{Many: f.inOptionOrder(v.Many)}
		}
	case "number":
		return v, f.checkNumber(v.One)
	case "boolean":
		if v.One != "true" && v.One != "false" {
			return v, `must be "true" or "false"`
		}
	case "date":
		if _, err := time.Parse(time.DateOnly, v.One); err != nil {
			return v, "must be a date as YYYY-MM-DD"
		}
	case "time":
		// time.Parse also takes a one-digit hour, which HH:MM does not.
		if _, err := time.Parse("15:04", v.One); err != nil || len(v.One) != len("15:04") {
			return v, "must be a time as HH:MM, from 00:00 to 23:59"
		}
	default:
		// A one-line input cannot show a line break, so a form saving the
		// value back would change it.
		if (input == "text" || input == "password") && strings.ContainsAny(v.One, "\r\n") {
			return v, "must be one line"
		}
		if msg := f.checkText(v.One); msg != "" || !sealed {
			return v, msg
		}
		return v, shortSecret(v.One)
	}
	return v, ""
}

// items returns the value as a list of strings.
func (v Value) items() []string {
	if v.IsList() {
		return v.Many
	}
	return []string{v.One}
}

// checkNumber returns why s is not a number the field takes, "" if it is.
func (f *Field) checkNumber(s string) string {
	if !numberSyntax.MatchString(s) {
		return "must be a number"
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return "must be a number"
	}
	if f.Min != nil && n < *f.Min {
		return fmt.Sprintf("must be at least %g", *f.Min)
	}
	if f.Max != nil && n > *f.Max {
		return fmt.Sprintf("must be at most %g", *f.Max)
	}
	return ""
}

// numberSyntax is the grammar of a number an HTML number input shows back
// as written: an optional minus, digits, an optional fraction with digits on
// both sides of the point, and an optional exponent. ParseFloat also takes
// "+5", ".5", "5.", "0x10", "1_000" and "Inf", which such an input would not
// show, so a form saving the value back would change it.
var numberSyntax = regexp.MustCompile(`^-?\d+(\.\d+)?([eE][+-]?\d+)?$`)

// inOptionOrder returns the chosen values in the order of the field's
// options. Every value is one of them.
func (f *Field) inOptionOrder(chosen []string) []string {
	out := make([]string, 0, len(chosen))
	for _, o := range f.Options {
		if slices.Contains(chosen, o.Value) {
			out = append(out, o.Value)
		}
	}
	return out
}

// checkText returns why s is too long for the field, "" if it is not.
func (f *Field) checkText(s string) string {
	if f.MaxLength > 0 && utf8.RuneCountInString(s) > f.MaxLength {
		return fmt.Sprintf("must be at most %d characters", f.MaxLength)
	}
	return ""
}
