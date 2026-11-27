package render

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// member is one file of a result tar.
type member struct {
	name string
	body string
}

// resultTar returns a result tar of the members, complete.
func resultTar(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range members {
		if err := AddFile(tw, m.name, int64(len(m.body)), strings.NewReader(m.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pngsOf returns a member for each layout, whose body is the layout's name.
func pngsOf(layouts ...string) []member {
	out := make([]member, 0, len(layouts))
	for _, l := range layouts {
		out = append(out, member{l + ".png", l})
	}
	return out
}

// pngSink returns a PNG callback that records what it was given, by layout.
func pngSink(got map[string]string) func(string, io.Reader) error {
	return func(layout string, r io.Reader) error {
		b, err := io.ReadAll(r)
		got[layout] = string(b)
		return err
	}
}

// TestReadResult checks what a result tar must hold to be read: a log and
// the layouts' PNGs, handed on as they stream, in any order, and nothing
// else.
func TestReadResult(t *testing.T) {
	t.Parallel()
	all := pngsOf(Layouts...)
	for _, tc := range []struct {
		name    string
		members []member
		wantLog string
		layouts int
		fetch   bool
		// err is a string the error must contain, or "" for none.
		err string
	}{
		{name: "full", members: append([]member{{logMember, "built\n"}}, all...), wantLog: "built\n", layouts: 4},
		{name: "log after the PNGs", members: append(slices.Clone(all), member{logMember, "late"}), wantLog: "late", layouts: 4},
		{name: "no log", members: all, layouts: 4},
		{name: "empty", members: nil},
		{name: "fetch failed", members: []member{{logMember, "warn: HTTP 401"}, {fetchFailedMember, ""}}, wantLog: "warn: HTTP 401", fetch: true},
		{name: "unknown member", members: []member{{logMember, "x"}, {"evil.sh", "rm"}}, wantLog: "x", err: `unexpected "evil.sh"`},
		{name: "unknown layout", members: []member{{"huge.png", "x"}}, err: `unexpected "huge.png"`},
		{name: "path member", members: []member{{"../full.png", "x"}}, err: "unexpected"},
		{
			name: "log cut", wantLog: strings.Repeat("x", maxLogBytes), layouts: 4,
			members: append([]member{{logMember, strings.Repeat("x", maxLogBytes+100<<10)}}, all...),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := map[string]string{}
			res, err := readResult(bytes.NewReader(resultTar(t, tc.members...)), pngSink(got))
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("error = %v, want one with %q", err, tc.err)
			}
			if res.log != tc.wantLog {
				t.Errorf("log = %d bytes, want %d", len(res.log), len(tc.wantLog))
			}
			if tc.err == "" && (res.layouts != tc.layouts || res.fetchFailed != tc.fetch) {
				t.Errorf("layouts %d, fetchFailed %v; want %d, %v", res.layouts, res.fetchFailed, tc.layouts, tc.fetch)
			}
			for layout, body := range got {
				if body != layout {
					t.Errorf("layout %q got body %q", layout, body)
				}
			}
		})
	}
}

// TestReadResultPNGLimit checks that a PNG whose header says it is over the
// limit is refused before any of it is read, so a build cannot make the
// server read or store an image without bound, and that one at the limit is
// not refused.
func TestReadResultPNGLimit(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "full.png", Mode: 0o600, Size: maxPNGBytes + 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	// The header only: a reader past it would find the end of the stream.
	called := false
	res, err := readResult(&buf, func(string, io.Reader) error { called = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "over the limit") {
		t.Errorf("error = %v, want one that says it is over the limit", err)
	}
	if called || res.layouts != 0 {
		t.Errorf("the PNG was handed on: called %v, layouts %d", called, res.layouts)
	}

	buf.Reset()
	tw = tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "full.png", Mode: 0o600, Size: maxPNGBytes, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(tw, zerosReader{}, maxPNGBytes); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var n int64
	res, err = readResult(&buf, func(_ string, r io.Reader) error {
		var err error
		n, err = io.Copy(io.Discard, r)
		return err
	})
	if err != nil || res.layouts != 1 || n != maxPNGBytes {
		t.Errorf("a PNG at the limit: %d bytes, %d layouts, %v", n, res.layouts, err)
	}
}

// zerosReader reads zeros without end.
type zerosReader struct{}

func (zerosReader) Read(p []byte) (int, error) { return len(p), nil }

// TestReadResultMalformed checks that a result cut short or not a tar is an
// error, with the log read before it kept, and that an error from the PNG
// callback ends the read and is returned as it is.
func TestReadResultMalformed(t *testing.T) {
	t.Parallel()
	whole := resultTar(t, append([]member{{logMember, "partial log"}}, pngsOf(Layouts...)...)...)
	for _, tc := range []struct {
		name string
		data []byte
		// log is what must have been read.
		log string
	}{
		{"not a tar", []byte(strings.Repeat("not a tar file, just text. ", 100)), ""},
		{"cut in the first header", whole[:200], ""},
		{"cut in the first PNG", whole[:1024+512+2], "partial log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := readResult(bytes.NewReader(tc.data), pngSink(map[string]string{}))
			if err == nil {
				t.Fatal("no error")
			}
			if res.log != tc.log {
				t.Errorf("log = %q, want %q", res.log, tc.log)
			}
		})
	}

	boom := errors.New("boom")
	res, err := readResult(bytes.NewReader(whole), func(string, io.Reader) error { return boom })
	if !errors.Is(err, boom) || res.layouts != 0 {
		t.Errorf("callback error: %v, %d layouts", err, res.layouts)
	}
}

// TestOutcome checks how a result becomes an error: a failed build is a
// failed fetch when the result said so, and a good build needs every layout.
func TestOutcome(t *testing.T) {
	t.Parallel()
	full := received{layouts: len(Layouts)}
	for _, tc := range []struct {
		name  string
		res   received
		built bool
		want  error
		text  string
	}{
		{name: "built", res: full, built: true},
		{name: "build failed", res: full, want: ErrBuildFailed},
		{name: "fetch failed", res: received{fetchFailed: true}, want: ErrFetchFailed},
		{name: "missing layouts", res: received{layouts: 3}, built: true, text: "3 of 4 layouts"},
		{name: "no layouts", built: true, text: "0 of 4 layouts"},
		{name: "too many layouts", res: received{layouts: 5}, built: true, text: "5 of 4 layouts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.res.outcome(tc.built)
			switch {
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("outcome = %v, want %v", err, tc.want)
			case tc.text != "" && (err == nil || !strings.Contains(err.Error(), tc.text)):
				t.Errorf("outcome = %v, want text %q", err, tc.text)
			case tc.want == nil && tc.text == "" && err != nil:
				t.Errorf("outcome = %v, want nil", err)
			}
		})
	}
}

// TestPrepareJob checks that a job is checked and encoded before it is sent
// by either client: a bad one is refused with its reason, and one over the
// size limit is refused as ErrRejected, since another try would be too.
func TestPrepareJob(t *testing.T) {
	t.Parallel()
	good := Job{Width: 800, Height: 480, Palette: "bw", Env: map[string]string{"FIELD_CITY": "Oslo"}}
	enc, err := prepareJob(good)
	if err != nil {
		t.Fatalf("prepareJob: %v", err)
	}
	if !strings.Contains(string(enc), `"FIELD_CITY":"Oslo"`) || bytes.HasSuffix(enc, []byte("\n")) {
		t.Errorf("encoded job = %q", enc)
	}
	for _, tc := range []struct {
		name string
		job  Job
		// rejected is whether the error matches ErrRejected.
		rejected bool
		text     string
	}{
		{"bad palette", Job{Width: 800, Height: 480, Palette: "No Good"}, false, "bad palette"},
		{"empty palette", Job{Width: 800, Height: 480}, false, "bad palette"},
		{"zero size", Job{Palette: "bw"}, false, "bad size"},
		{"huge size", Job{Width: 100000, Height: 480, Palette: "bw"}, false, "bad size"},
		{"bad env name", Job{Width: 800, Height: 480, Palette: "bw", Env: map[string]string{"LD_PRELOAD": "x"}}, false, "environment variable"},
		{
			"env over the limit",
			Job{Width: 800, Height: 480, Palette: "bw", Env: map[string]string{"FIELD_BIG": strings.Repeat("x", maxJobBytes)}},
			true, "over the limit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := prepareJob(tc.job)
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error = %v, want one with %q", err, tc.text)
			}
			if got := errors.Is(err, ErrRejected); got != tc.rejected {
				t.Errorf("errors.Is(err, ErrRejected) = %v, want %v", got, tc.rejected)
			}
		})
	}
}
