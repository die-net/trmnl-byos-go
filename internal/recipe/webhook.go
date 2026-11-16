package recipe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// MaxDataBytes bounds the JSON an instance keeps from pushes, and so how
// much a build's .trmnlp.yml carries.
const MaxDataBytes = 64 << 10

// ErrDataTooBig means a push would leave an instance with more than
// MaxDataBytes of data.
var ErrDataTooBig = errkind.New(errkind.ErrTooBig, "instance data would be over the limit")

// Merge strategies for a push, as TRMNL names them.
const (
	// StrategyReplace makes the push the instance's data.
	StrategyReplace = "replace"
	// StrategyDeepMerge merges the push into the data, key by key and
	// object by object. Anything else, arrays included, is replaced.
	StrategyDeepMerge = "deep_merge"
	// StrategyStream appends the push's arrays to the data's, and keeps the
	// newest values up to a limit.
	StrategyStream = "stream"
)

// Data is a JSON object whose shape is not known. Pushed data is the one
// place the server decodes into maps and slices, since it has no fixed
// fields to decode into. Numbers are json.Number, which keeps an integer
// exact through the merge.
type Data = map[string]any

// ParseData decodes a JSON object from r. It refuses anything else,
// including text after the object, with an error that wraps errkind.ErrInvalid.
func ParseData(r io.Reader) (Data, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var d Data
	if err := dec.Decode(&d); err != nil || d == nil {
		return nil, fmt.Errorf("%w: data is not a JSON object", errkind.ErrInvalid)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: data has text after the JSON object", errkind.ErrInvalid)
	}
	return d, nil
}

// DeepMerge returns base with add merged in: an object in both is merged
// the same way, and any other value in add replaces the one in base. It
// changes base and shares add's values.
func DeepMerge(base, add Data) Data {
	if base == nil {
		base = Data{}
	}
	for k, v := range add {
		if from, ok := v.(map[string]any); ok {
			if into, ok := base[k].(map[string]any); ok {
				base[k] = DeepMerge(into, from)
				continue
			}
		}
		base[k] = v
	}
	return base
}

// Stream returns base with each array of add appended to the array of the
// same key, and a value that is not an array in both replacing the old one.
// An array that grows past limit loses its oldest values, from the front. A
// limit of 0 means no limit. It changes base.
func Stream(base, add Data, limit int) Data {
	if base == nil {
		base = Data{}
	}
	for k, v := range add {
		next, ok := v.([]any)
		if !ok {
			base[k] = v
			continue
		}
		if prev, ok := base[k].([]any); ok {
			next = append(prev[:len(prev):len(prev)], next...)
		}
		if limit > 0 && len(next) > limit {
			next = next[len(next)-limit:]
		}
		base[k] = next
	}
	return base
}

// PushParams is a push of data to an instance.
type PushParams struct {
	InstanceID int64
	// Strategy is one of the Strategy constants, or "" for StrategyReplace.
	Strategy string
	// Add is the pushed object.
	Add Data
	// StreamLimit is the longest an array may be after a StrategyStream
	// push, or 0 for no limit.
	StreamLimit int
}

// PushData merges a push into the instance's data and saves the result,
// which it returns as JSON. It returns ErrNoInstance for an unknown
// instance, an error wrapping errkind.ErrInvalid for an unknown strategy, a trmnl
// key, or a negative limit, and ErrDataTooBig for a result over
// MaxDataBytes. The data is read and written in one transaction.
func PushData(ctx context.Context, client *db.Client, p PushParams, now time.Time) ([]byte, error) {
	if _, ok := p.Add["trmnl"]; ok {
		return nil, fmt.Errorf("%w: the trmnl key is the server's", errkind.ErrInvalid)
	}
	if p.StreamLimit < 0 {
		return nil, fmt.Errorf("%w: stream_limit is negative", errkind.ErrInvalid)
	}
	var out []byte
	err := client.ChangeTx(ctx, func(q *db.Queries) error {
		if _, err := q.GetInstance(ctx, p.InstanceID); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoInstance, p.InstanceID)
		} else if err != nil {
			return fmt.Errorf("get instance: %w", err)
		}
		row, err := q.GetInstanceData(ctx, p.InstanceID)
		stored := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("get instance data: %w", err)
		}
		var merged Data
		switch p.Strategy {
		case "", StrategyReplace:
			merged = p.Add
		case StrategyDeepMerge, StrategyStream:
			base := Data{}
			if stored {
				if base, err = ParseData(strings.NewReader(row.Data)); err != nil {
					return fmt.Errorf("stored data of instance %d: %w", p.InstanceID, err)
				}
			}
			if p.Strategy == StrategyStream {
				merged = Stream(base, p.Add, p.StreamLimit)
			} else {
				merged = DeepMerge(base, p.Add)
			}
		default:
			return fmt.Errorf("%w: unknown merge_strategy %q", errkind.ErrInvalid, p.Strategy)
		}
		if out, err = json.Marshal(merged); err != nil {
			return fmt.Errorf("encode data: %w", err)
		}
		if len(out) > MaxDataBytes {
			return fmt.Errorf("%w of %d bytes", ErrDataTooBig, MaxDataBytes)
		}
		// updated_at has to rise with every push. A build records the
		// updated_at it read, and the data is newer than the build only if
		// its updated_at is greater. A push in the same second as the last
		// would otherwise look like the data the build read, and go
		// unshown until the next refresh.
		updated := now.Unix()
		if stored && updated <= row.UpdatedAt {
			updated = row.UpdatedAt + 1
		}
		err = q.SaveInstanceData(ctx, db.SaveInstanceDataParams{InstanceID: p.InstanceID, Data: string(out), UpdatedAt: updated})
		if err != nil {
			return fmt.Errorf("save instance data: %w", err)
		}
		return nil
	})
	return out, err
}

// InstanceData returns the JSON an instance's pushes left, or {} if there
// are none. It returns ErrNoInstance for an unknown instance. The instance
// and its data are read in one snapshot, so an instance deleted between the
// two reads, which takes its data with it, is not found rather than {}.
func InstanceData(ctx context.Context, client *db.Client, id int64) ([]byte, error) {
	var out []byte
	err := client.ReadTx(ctx, func(q *db.Queries) error {
		if _, err := q.GetInstance(ctx, id); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoInstance, id)
		} else if err != nil {
			return fmt.Errorf("get instance: %w", err)
		}
		row, err := q.GetInstanceData(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			out = []byte("{}")
			return nil
		}
		if err != nil {
			return fmt.Errorf("get instance data: %w", err)
		}
		out = []byte(row.Data)
		return nil
	})
	return out, err
}

// WithData returns the project with the instance's pushed JSON as the top
// level of .trmnlp.yml's variables. trmnlp does not run variables through
// Liquid, so a string in it is only data, however it reads. Numbers are
// written as numbers. It returns an error if data is not a JSON object.
func (p Project) WithData(data string) (Project, error) {
	d, err := ParseData(strings.NewReader(data))
	if err != nil {
		return p, err
	}
	delete(d, "trmnl")
	v := p.config.Variables.clone()
	v.Data = yamlValue(d).(map[string]any)
	p.config.Variables = v
	return p, nil
}

// yamlValue returns v, decoded from JSON with UseNumber, with each
// json.Number as an int64, a uint64 or a float64, which the YAML encoder writes as a
// number. The encoder would write a json.Number as a string.
func yamlValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			v[k] = yamlValue(e)
		}
	case []any:
		for i, e := range v {
			v[i] = yamlValue(e)
		}
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i
		}
		if u, err := strconv.ParseUint(v.String(), 10, 64); err == nil {
			return u
		}
		if f, err := v.Float64(); err == nil {
			return f
		}
		return v.String()
	}
	return v
}
