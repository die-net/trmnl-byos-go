package errkind

import (
	"errors"
	"fmt"
	"testing"
)

func TestNew(t *testing.T) {
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
