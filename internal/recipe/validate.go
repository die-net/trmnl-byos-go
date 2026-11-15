package recipe

import (
	"fmt"
	"maps"
	"math"
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

// MinSecretLen is the fewest characters a password field's value may have.
// A shorter value is easy to guess, so sealing it protects nothing, and
// redacting it from a build log would hide every place its characters
// appear. Such a value can be a plain field.
const MinSecretLen = 6

// checkValues checks values against the recipe's field definitions and
// returns them with a single string for a multi-select turned into a list.
// Every problem is reported at once, in a FieldErrors. A value that is empty
// clears its field, so only its being required is checked. When creating,
// each required field also needs a value, given or not.
func checkValues(fields []Field, values map[string]Value, creating bool) (map[string]Value, error) {
	byName := fieldsByName(fields)
	bad := FieldErrors{}
	out := make(map[string]Value, len(values))
	for k, v := range values {
		f := byName[k]
		switch {
		case f == nil:
			bad[k] = "no such field"
		case v.Empty():
			out[k] = v
		default:
			var msg string
			if out[k], msg = f.check(v); msg != "" {
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
	if len(bad) > 0 {
		return nil, bad
	}
	return out, nil
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
// and why it is not acceptable for the field, "" if it is. v is not empty.
func (f *Field) check(v Value) (Value, string) {
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
	switch input {
	case "select":
		for _, s := range v.items() {
			if !slices.ContainsFunc(f.Options, func(o Option) bool { return o.Value == s }) {
				return v, fmt.Sprintf("%q is not one of the options", s)
			}
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
		return v, f.checkText(v.One, input)
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

// checkText returns why s is too long, or for a password too short, for
// the field, "" if it is not.
func (f *Field) checkText(s, input string) string {
	n := utf8.RuneCountInString(s)
	if f.MaxLength > 0 && n > f.MaxLength {
		return fmt.Sprintf("must be at most %d characters", f.MaxLength)
	}
	if input == "password" && n < MinSecretLen {
		return fmt.Sprintf("must be at least %d characters", MinSecretLen)
	}
	return ""
}
