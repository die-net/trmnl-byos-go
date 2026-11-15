package upload

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

func TestReader(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection reset")
	tooBig := &http.MaxBytesError{Limit: 5}
	for _, tc := range []struct {
		name       string
		r          io.Reader
		wantBody   string
		wantCutOff bool
		wantErr    error // The error the read must be, when it is not ErrCutOff.
	}{
		{"ends cleanly", strings.NewReader("hello"), "hello", false, nil},
		{"unexpected EOF", io.MultiReader(strings.NewReader("hel"), iotest.ErrReader(io.ErrUnexpectedEOF)), "hel", true, io.ErrUnexpectedEOF},
		{"other error", io.MultiReader(strings.NewReader("hel"), iotest.ErrReader(boom)), "hel", true, boom},
		{"body over its limit", io.MultiReader(strings.NewReader("hel"), iotest.ErrReader(tooBig)), "hel", false, tooBig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := io.ReadAll(Reader(tc.r))
			if string(got) != tc.wantBody {
				t.Errorf("read %q, want %q", got, tc.wantBody)
			}
			if gotCutOff := errors.Is(err, ErrCutOff); gotCutOff != tc.wantCutOff || errors.Is(err, errkind.ErrInvalid) != tc.wantCutOff {
				t.Fatalf("error %v: cut off = %v, want %v", err, gotCutOff, tc.wantCutOff)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error %v does not keep the cause %v", err, tc.wantErr)
			}
			if tc.wantCutOff && err.Error() != ErrCutOff.Error() {
				t.Errorf("message %q, want the plain %q", err, ErrCutOff)
			}
			if tc.wantErr == nil && err != nil {
				t.Errorf("a clean end gave %v", err)
			}
		})
	}
}

func TestCutOffKeepsLimit(t *testing.T) {
	t.Parallel()
	tooBig := &http.MaxBytesError{Limit: 5}
	if err := CutOff(tooBig); errors.Is(err, ErrCutOff) || !errors.Is(err, tooBig) {
		t.Errorf("CutOff(MaxBytesError) = %v, want it unchanged", err)
	}
	if err := CutOff(nil); err != nil {
		t.Errorf("CutOff(nil) = %v", err)
	}
}
