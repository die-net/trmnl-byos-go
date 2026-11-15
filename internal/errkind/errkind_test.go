package errkind

import (
	"errors"
	"fmt"
	"testing"
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
