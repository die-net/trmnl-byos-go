package adminapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/die-net/trmnl-byos-go/internal/recipe"
)

// Limits on pushes to one instance: a burst of pushLimit, then pushPerHour
// an hour. TRMNL's own is 12 an hour, with 30 for its paid plan.
const (
	pushLimit   = 10
	pushPerHour = 60
)

// pushLimiters holds a rate limiter per instance, in memory. An instance
// that is deleted leaves its limiter, a few dozen bytes, until the server
// restarts.
type pushLimiters struct {
	mu sync.Mutex
	by map[int64]*rate.Limiter
}

// allow takes a push for instance id at now. It returns how long the caller
// has to wait when there is no room, which is 0 when it was taken.
func (l *pushLimiters) allow(id int64, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.by == nil {
		l.by = map[int64]*rate.Limiter{}
	}
	lim := l.by[id]
	if lim == nil {
		lim = rate.NewLimiter(rate.Every(time.Hour/pushPerHour), pushLimit)
		l.by[id] = lim
	}
	// One token is never over the burst, so the reservation is always OK.
	res := lim.ReserveN(now, 1)
	wait := res.DelayFrom(now)
	if wait > 0 {
		res.CancelAt(now)
	}
	return wait
}

// pushRequest is the body of a push to a custom plugin, in TRMNL's shape.
type pushRequest struct {
	// MergeVariables is the data, which must be a JSON object.
	MergeVariables json.RawMessage `json:"merge_variables"`
	// MergeStrategy is replace (the default), deep_merge or stream.
	MergeStrategy string `json:"merge_strategy"`
	// StreamLimit is the longest an array may get under the stream
	// strategy. Zero or absent means no limit.
	StreamLimit int `json:"stream_limit"`
}

// dataResponse is the response to a push or a read, as TRMNL's: the data
// the instance has now.
type dataResponse struct {
	MergeVariables json.RawMessage `json:"merge_variables"`
}

// pushData is POST /api/custom_plugins/{id}.
func (s *Server) pushData(w http.ResponseWriter, r *http.Request) (dataResponse, error) {
	id, err := pathID(r, recipe.ErrNoInstance)
	if err != nil {
		return dataResponse{}, err
	}
	ctx := r.Context()
	// An unknown instance is answered before the limiter, so a caller
	// cannot make it keep a limiter for any ID.
	if _, err := s.db.Read.GetInstance(ctx, id); errors.Is(err, sql.ErrNoRows) {
		return dataResponse{}, fmt.Errorf("%w: %d", recipe.ErrNoInstance, id)
	} else if err != nil {
		return dataResponse{}, fmt.Errorf("get instance: %w", err)
	}
	var req pushRequest
	if err := decode(w, r, &req, false); err != nil {
		return dataResponse{}, err
	}
	add, err := recipe.ParseData(bytes.NewReader(req.MergeVariables))
	if err != nil {
		return dataResponse{}, fmt.Errorf("merge_variables: %w", err)
	}
	if wait := s.pushes.allow(id, s.now()); wait > 0 {
		return dataResponse{}, &statusError{
			status: http.StatusTooManyRequests,
			msg:    "too many pushes to this instance",
			header: http.Header{"Retry-After": {strconv.Itoa(int(math.Ceil(wait.Seconds())))}},
		}
	}
	out, err := recipe.PushData(ctx, s.db, recipe.PushParams{
		InstanceID: id, Strategy: req.MergeStrategy, Add: add, StreamLimit: req.StreamLimit,
	}, s.now())
	if err != nil {
		return dataResponse{}, err
	}
	return dataResponse{MergeVariables: out}, nil
}

// getData is GET /api/custom_plugins/{id}.
func (s *Server) getData(_ http.ResponseWriter, r *http.Request) (dataResponse, error) {
	id, err := pathID(r, recipe.ErrNoInstance)
	if err != nil {
		return dataResponse{}, err
	}
	out, err := recipe.InstanceData(r.Context(), s.db, id)
	if err != nil {
		return dataResponse{}, err
	}
	return dataResponse{MergeVariables: out}, nil
}
