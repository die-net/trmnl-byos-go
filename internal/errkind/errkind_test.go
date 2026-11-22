package errkind

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	t.Parallel()
	sentinel := New(ErrNotFound, "no such widget")
	err := fmt.Errorf("get: %w: 7", sentinel)
	if !errors.Is(err, sentinel) || !errors.Is(err, ErrNotFound) {
		t.Errorf("%v is not both the sentinel and NotFound", err)
	}
	if errors.Is(err, ErrInvalid) {
		t.Errorf("%v is Invalid", err)
	}
	if sentinel.Error() != "no such widget" {
		t.Errorf("message = %q", sentinel.Error())
	}
}

func TestWrap(t *testing.T) {
	t.Parallel()
	cause := errors.New("zip: checksum error")
	err := fmt.Errorf("save: %w", Wrap(ErrInvalid, "the file is damaged", cause))
	if !errors.Is(err, ErrInvalid) || !errors.Is(err, cause) {
		t.Errorf("%v does not keep both the kind and the cause", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("%v is NotFound", err)
	}
	if got := errors.Unwrap(err).Error(); got != "the file is damaged" {
		t.Errorf("message = %q, want the plain message without the cause", got)
	}
}

func TestEnded(t *testing.T) {
	t.Parallel()
	live := context.Background()
	canceled, cancel := context.WithCancel(live)
	cancel()
	expired, cancelExpired := context.WithDeadline(live, time.Unix(0, 0))
	defer cancelExpired()
	other := errors.New("disk full")
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"live context, cancel error", live, context.Canceled, false},
		{"canceled, wrapped cancel", canceled, fmt.Errorf("begin: %w", context.Canceled), true},
		{"canceled, joined cancel", canceled, errors.Join(context.Canceled, other), true},
		{"canceled, other error", canceled, other, false},
		{"canceled, nil error", canceled, nil, false},
		{"canceled, deadline error", canceled, context.DeadlineExceeded, false},
		{"expired, wrapped deadline", expired, fmt.Errorf("commit: %w", context.DeadlineExceeded), true},
	} {
		if got := Ended(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: Ended = %v, want %v", tc.name, got, tc.want)
		}
	}
}
