package render

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// BuildCommand is the trmnl-byos subcommand that runs one build inside a
// sandbox. The server starts it as the sandbox's command, never a person.
const BuildCommand = "render-build"

// The build command's exit statuses, which Sandbox.Build reads. The command
// sets them before anything of the recipe runs, or after the build, from
// what it found, and a recipe's code cannot change them: the command makes
// itself not dumpable first, so no process of the build may trace it. Any
// other status, or a signal, is a build that ended some other way, which
// counts against the build.
const (
	// exitBuilt means trmnlp drew every layout. The result is on stdout.
	exitBuilt = 0
	// exitBuildFailed means trmnlp failed, or its data fetch did. The
	// result, with the log and a fetchFailedMember if the fetch failed, is
	// on stdout.
	exitBuildFailed = 3
	// exitRejected means the job or its project is malformed, as the shim's
	// 400. Stderr says why.
	exitRejected = 4
	// exitUnavailable means the build could not start for a reason of the
	// renderer's, before anything of the recipe ran, as the shim's 503.
	// Stderr says why.
	exitUnavailable = 5
	// exitUsage means the command was started wrongly.
	exitUsage = 2
)

// StartedFD is the descriptor of the pipe on which the build command says it
// has started, before anything of the recipe runs. A sandbox that ends
// before the command says so failed for the renderer's reasons, not the
// recipe's, such as a first stage killed by an AppArmor profile.
const StartedFD = 3

// RunBuildCommand runs BuildCommand: it reads a request tar from stdin,
// the job and its project, builds it in a fresh directory under TMPDIR,
// which in the sandbox is a tmpfs of its own with a size limit, and writes
// the result tar to stdout. args are its flags: -trmnlp, the path to
// trmnlp, and -xvfb, the path to Xvfb. The
// environment is the build's, which the server made, and every tool the
// build runs inherits it. It writes one byte to started, then closes it,
// once it has protected itself and before it reads the job. It returns
// the exit status, and an error that says why, which the caller writes to
// stderr for the server to read. A failed build has the reason in its log,
// and an error only when the result could not be written.
func RunBuildCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, started io.WriteCloser) (int, error) {
	fs := flag.NewFlagSet(BuildCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	trmnlp := fs.String("trmnlp", "", "path to the trmnlp command")
	xvfb := fs.String("xvfb", "", "path to Xvfb")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *trmnlp == "" || *xvfb == "" {
		return exitUsage, fmt.Errorf("usage: %s -trmnlp path -xvfb path; the server runs it in a build's sandbox", BuildCommand)
	}
	// The build runs as this process's user, so without this a process of
	// the build could trace it, read its memory, or take its descriptors,
	// stdout included, and forge a result or an exit status.
	if err := ProtectSelf(); err != nil {
		return exitUnavailable, fmt.Errorf("protect the build command: %w", err)
	}
	// Closed at once, so nothing the build runs inherits it.
	_, err := started.Write([]byte{1})
	if cerr := started.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return exitUnavailable, fmt.Errorf("say the build command started: %w", err)
	}
	dir, err := os.MkdirTemp("", "build-")
	if err != nil {
		return exitUnavailable, fmt.Errorf("create build dir: %w", err)
	}
	job, err := readRequest(stdin, filepath.Join(dir, "project"))
	if err != nil {
		if errors.Is(err, errInvalidRequest) {
			return exitRejected, err
		}
		return exitUnavailable, err
	}
	// Xvfb's output is kept apart, so stderr holds only this command's own
	// reasons, and shown when Xvfb did not start.
	xvfbOut := &capped{}
	b := builder{trmnlp: *trmnlp, xvfb: *xvfb, env: os.Environ(), xvfbOut: xvfbOut}
	buildErr := b.build(ctx, dir, job)
	// Xvfb starts, and trmnlp is started, before anything of the recipe
	// runs, so a failure of either is the renderer's.
	if errors.Is(buildErr, errNoDisplay) {
		if out := strings.TrimSpace(xvfbOut.String()); out != "" {
			buildErr = fmt.Errorf("%w: %s", buildErr, out)
		}
		return exitUnavailable, buildErr
	}
	if errors.Is(buildErr, errStartTrmnlp) {
		return exitUnavailable, buildErr
	}
	// A directory the build made unreadable, or replaced, fails the
	// build, with the reason after its log, as the shim does.
	var res *result
	root, err := os.OpenRoot(dir)
	if err != nil {
		res, err = &result{}, fmt.Errorf("open the build directory: %w", err)
	} else {
		defer root.Close()
		res, err = openResult(root, buildErr == nil)
	}
	defer res.close()
	note := ""
	if err != nil {
		note = "\n" + err.Error() + "\n"
		if buildErr == nil {
			buildErr = fmt.Errorf("%w: %w", ErrBuildFailed, err)
		}
	}
	if err := writeResult(stdout, res, note, errors.Is(buildErr, ErrFetchFailed)); err != nil {
		return exitBuildFailed, fmt.Errorf("write result: %w", err)
	}
	if buildErr != nil {
		return exitBuildFailed, nil
	}
	return exitBuilt, nil
}
