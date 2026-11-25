package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/sandbox"

	"github.com/die-net/trmnl-byos-go/internal/cache"
	"github.com/die-net/trmnl-byos-go/internal/render"
)

// errDoctorFailed is what doctor returns when a check that builds need
// failed, which makes it exit non-zero.
var errDoctorFailed = errors.New("a check failed: builds cannot run, or are not kept apart as they should be")

// report writes doctor's lines and counts the failures.
type report struct {
	out io.Writer
	// failures counts the failed checks that builds need, which make
	// doctor exit non-zero.
	failures int
	// generic counts the library's own checks that failed, which doctor
	// lists without counting (see doctorSandbox).
	generic int
	err     error
}

// line writes one check's outcome: pass, fail, warn (a gap the operator may
// close, which builds do not need closed), n/a (not enforceable here) or
// skip. A fail counts.
func (r *report) line(status, name, detail string) {
	if status == "fail" {
		r.failures++
	}
	r.write(status, name, detail)
}

func (r *report) write(status, name, detail string) {
	if r.err == nil {
		_, r.err = fmt.Fprintf(r.out, "%-4s  %s: %s\n", status, name, detail)
	}
}

// result writes a sandbox check's result. A failure counts only if counted
// is set.
func (r *report) result(res sandbox.Result, counted bool) {
	status := map[sandbox.Status]string{
		sandbox.Pass: "pass", sandbox.Fail: "fail", sandbox.NotEnforceable: "n/a", sandbox.Skipped: "skip",
	}[res.Status]
	if status == "" {
		status = res.Status.String()
	}
	switch {
	case counted:
		r.line(status, res.Name, res.Detail)
	case status == "fail":
		r.generic++
		r.write(status, res.Name, res.Detail)
	default:
		r.write(status, res.Name, res.Detail)
	}
}

// doctor checks that builds can run as serve with the same flags would run
// them, and what keeps them apart, and writes one line per check to out.
// It returns errDoctorFailed when a check builds need failed.
func doctor(ctx context.Context, args []string, out io.Writer) error {
	fs, f := newServeFlags("doctor")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if fs.NArg() != 0 {
		return errors.New("usage: trmnl-byos doctor [serve's flags]")
	}
	if err := f.check(); err != nil {
		return err
	}
	r := &report{out: out}
	if f.renderURL != "" {
		doctorShim(ctx, r, f.renderURL)
	} else if err := doctorSandbox(ctx, r, f); err != nil {
		return err
	}
	if r.err != nil {
		return r.err
	}
	if r.failures > 0 {
		return errDoctorFailed
	}
	return nil
}

// doctorShimTimeout bounds the request to the renderer.
const doctorShimTimeout = 10 * time.Second

// doctorShim checks that the renderer at url answers. The renderer checks
// its own isolation when it starts, so the rest is in its log.
func doctorShim(ctx context.Context, r *report, url string) {
	r.line("pass", "render.path", "builds go to the renderer at "+url+", from -render-url")
	ctx, cancel := context.WithTimeout(ctx, doctorShimTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/", http.NoBody)
	if err != nil {
		r.line("fail", "renderer.answers", fmt.Sprintf("-render-url %q: %v", url, err))
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.line("fail", "renderer.answers", fmt.Sprintf("%v; start trmnl-render, or point -render-url at it", err))
		return
	}
	_ = resp.Body.Close()
	// The shim serves only POST /build, so any status means it is there.
	r.line("pass", "renderer.answers", "GET / answered "+resp.Status)
	r.line("skip", "renderer.isolation", "trmnl-render checks its own user, the places it can write and the processes of its user when it starts, and refuses to start if they are wrong. Its log says why. The network, its limits and what it can read are the deployment's to give (see Renderer under its own user account in docs/setup.md)")
}

// doctorSandbox checks that each build's sandbox can start on this host,
// that the tools builds run are there, and what the sandbox keeps from a
// build. It returns an error only when it could not check.
func doctorSandbox(ctx context.Context, r *report, f *serveFlags) error {
	r.line("pass", "render.path", "each build runs in a sandbox on this host, since there is no -render-url")
	rep, err := render.ProbeSandbox(ctx)
	if rep.Backend != "" {
		detail := rep.Backend
		if rep.Container != "" {
			detail += ", inside " + rep.Container
		}
		r.line("pass", "sandbox.backend", detail)
	}
	for _, c := range rep.Checks {
		r.result(c, true)
	}
	if err != nil {
		r.line("fail", "sandbox.start", err.Error())
		return nil
	}
	if len(rep.Unsupported) > 0 {
		r.line("n/a", "sandbox.unsupported", strings.Join(rep.Unsupported, ", "))
	}

	tools := render.FindTools(ctx, os.Getenv("PATH"))
	for _, tool := range tools.List {
		name := "tool." + tool.Name
		if tool.Path == "" {
			status := "warn"
			if tool.Required {
				status = "fail"
			}
			r.line(status, name, fmt.Sprintf("not found on PATH; it %s. Install it with the system's package manager", tool.Use))
			continue
		}
		version, err := render.ToolVersion(ctx, tool)
		if err != nil {
			r.line("fail", name, fmt.Sprintf("%s: %v", tool.Path, err))
			continue
		}
		if version != "" {
			version = ", " + version
		}
		r.line("pass", name, tool.Path+version)
	}
	if tools.GemErr != nil {
		r.line("fail", "tool.gems", tools.GemErr.Error()+"; trmnlp cannot load its gems without Ruby's gem path")
	}
	for _, dir := range tools.HiddenGems {
		r.line("fail", "tool.gems", "Ruby's gem path has "+dir+", which is or holds a person's home, so builds cannot see it. Install trmnlp's gems in a directory of their own, such as system-wide")
	}
	for _, p := range tools.Read {
		r.line("pass", "sandbox.read", "shown read-only to builds, though under a path the sandbox covers: "+p)
	}
	for _, p := range tools.Cover {
		r.line("pass", "sandbox.cover", "hidden from builds, though inside a path shown to them: "+p)
	}
	if len(tools.Missing()) > 0 || tools.GemErr != nil || len(tools.HiddenGems) > 0 {
		return nil
	}
	// doctor makes nothing in the data directory, which need not exist
	// yet: its sandboxes' homes and its proxy's asset cache are throwaway
	// ones, and the self-test only tunnels.
	if _, err := os.Stat(f.data); err != nil {
		r.line("warn", "server.data", fmt.Sprintf("%v. serve makes it when it first starts. Pass the -data directory the server uses", err))
	}
	scratch, err := os.MkdirTemp("", "trmnl-byos-doctor-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			r.line("warn", "doctor.cleanup", err.Error())
		}
	}()
	if scratch, err = filepath.EvalSymlinks(scratch); err != nil {
		return err
	}
	assets, err := cache.New(filepath.Join(scratch, "cache"))
	if err != nil {
		return err
	}
	sb, err := f.sandbox(assets, filepath.Join(scratch, "tmp"))
	if err != nil {
		return err
	}
	home := filepath.Join(scratch, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return err
	}
	spec := sb.Spec(tools, home)
	want, err := expect(f)
	if err != nil {
		return err
	}
	// The library's own checks are listed, and only the ones that check
	// what the server claims count: some of the rest fail in every sandbox
	// v0.1.0 makes, such as proc.visible, which counts the sandbox's own
	// pid 1 as a process outside it, and covers:/var, which counts the
	// sandbox's own /var/tmp. Its limits checks look at the sandbox Check
	// makes, which has neither the build's cgroup nor its watchdog, so
	// the build's limits have lines of their own below.
	results, err := sandbox.Check(ctx, &spec, want)
	for _, res := range results {
		r.result(res, strings.HasPrefix(res.Name, "expect."))
	}
	if err != nil {
		r.line("fail", "sandbox.check", err.Error())
	}
	doctorLimits(r, f, sb)
	results, err = sb.SelfTest(ctx, refusedTargets(f.listen), []string{"trmnl.com:443"})
	for _, res := range results {
		r.result(res, true)
	}
	if err != nil {
		r.line("fail", "proxy.selftest", err.Error())
	}
	if r.generic > 0 {
		r.write("note", "sandbox.check", fmt.Sprintf("%d of the sandbox library's own checks failed. They are listed above, and do not change doctor's exit status, since some fail in every sandbox of this version of the library", r.generic))
	}
	return nil
}

// doctorLimits says what bounds a build's memory and tasks.
func doctorLimits(r *report, f *serveFlags, sb *render.Sandbox) {
	if f.renderCgroup == "" {
		r.line("pass", "build.memory", fmt.Sprintf("a watchdog stops a build whose processes hold over %d MiB, checked 4 times a second. It misses a spike between checks, and the tmpfs a build writes; -render-cgroup bounds both", f.renderMemory))
		r.line("warn", "build.tasks", "nothing limits how many processes a build starts, until its timeout ends it; -render-cgroup limits them")
		return
	}
	if err := sb.CheckCgroup(); err != nil {
		r.line("fail", "build.cgroup", err.Error())
		return
	}
	r.line("pass", "build.memory", fmt.Sprintf("each build runs in a cgroup of its own in %s, with memory.max %d MiB and no swap", f.renderCgroup, f.renderMemory))
	r.line("pass", "build.tasks", "each build's cgroup has pids.max "+strconv.Itoa(render.MaxTasks))
}

// expect is what a build's sandbox must keep from it, beyond the library's
// own checks: the data directory and the asset cache unread, the server's
// listener unreached, and the build cgroup, if any, unwritten.
func expect(f *serveFlags) (sandbox.Expect, error) {
	var want sandbox.Expect
	for _, p := range []string{f.data, f.cacheDir} {
		abs, err := filepath.Abs(p)
		if err != nil {
			return want, err
		}
		want.Unreadable = append(want.Unreadable, abs)
	}
	if addr := loopbackAddr(f.listen); addr != "" {
		want.Unreachable = append(want.Unreachable, "tcp:"+addr)
	}
	// A build that could write the cgroup could raise its own limits, or
	// kill the server's.
	if f.renderCgroup != "" {
		want.Unwritable = append(want.Unwritable, f.renderCgroup)
	}
	return want, nil
}

// loopbackAddr returns listen, a listen address such as :8080, as an
// address on 127.0.0.1 when it has no host, or "" if it is not host:port.
func loopbackAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// refusedTargets are what the build's proxy must refuse: the server's own
// listener, on both loopbacks and on each address of the host's, which a build
// could otherwise reach through the proxy, and the cloud metadata address.
func refusedTargets(listen string) []string {
	out := []string{"169.254.169.254:80"}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return out
	}
	out = append(out, net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("::1", port))
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && !ip.IP.IsLoopback() && !ip.IP.IsLinkLocalUnicast() {
			out = append(out, net.JoinHostPort(ip.IP.String(), port))
		}
	}
	return out
}
