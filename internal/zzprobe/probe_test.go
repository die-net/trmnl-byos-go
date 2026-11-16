// Package zzprobe is a throwaway probe of what code run by the review job's
// Claude Code sandbox can reach. It never prints a secret value: only names,
// presence, lengths and counts.
package zzprobe

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

var shapes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"anthropic-key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
	{"github-token", regexp.MustCompile(`gh[opsur]_[A-Za-z0-9]{30,}`)},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.`)},
	{"oauth-env-name", regexp.MustCompile(`CLAUDE_CODE_OAUTH_TOKEN`)},
}

// shapeCounts counts secret-shaped strings in s, by kind, without keeping them.
func shapeCounts(s string) string {
	var parts []string
	for _, sh := range shapes {
		if n := len(sh.re.FindAllStringIndex(s, -1)); n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", sh.name, n))
		}
	}
	return strings.Join(parts, ",")
}

var sensitive = regexp.MustCompile(`^(CLAUDE_CODE_OAUTH_TOKEN|ANTHROPIC_.*|GITHUB_TOKEN|GH_TOKEN|ACTIONS_.*|INPUT_.*|OVERRIDE_GITHUB_TOKEN|DEFAULT_WORKFLOW_TOKEN|ALL_INPUTS|.*TOKEN.*|.*SECRET.*|.*KEY.*)$`)

type report struct{ b strings.Builder }

func (r *report) p(format string, a ...any) { fmt.Fprintf(&r.b, "PROBE "+format+"\n", a...) }

func TestProbe(t *testing.T) {
	tag := os.Getenv("PROBE_TAG")
	if tag == "" {
		tag = "sandbox"
	}
	r := &report{}
	r.p("tag=%s", tag)
	identity(r)
	environment(r)
	procs(r)
	files(r)
	network(r)
	writes(r)

	out := r.b.String()
	fmt.Print(out)
	dst := "/home/runner/work/_temp/review/scratch/probe-" + tag + ".txt"
	fmt.Printf("PROBE write %s: %v\n", dst, os.WriteFile(dst, []byte(out), 0o644))
}

func identity(r *report) {
	wd, _ := os.Getwd()
	host, _ := os.Hostname()
	r.p("id uid=%d gid=%d pid=%d ppid=%d host=%q wd=%q home=%q", os.Getuid(), os.Getgid(), os.Getpid(), os.Getppid(), host, wd, os.Getenv("HOME"))
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			for _, k := range []string{"NSpid:", "NoNewPrivs:", "Seccomp:", "CapEff:", "CapBnd:"} {
				if strings.HasPrefix(l, k) {
					r.p("status %s", strings.Join(strings.Fields(l), " "))
				}
			}
		}
	}
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) > 5 && (f[4] == "/" || f[4] == "/home/runner" || f[4] == "/home/runner/work/_temp/review/scratch" || f[4] == "/tmp" || f[4] == "/proc") {
				r.p("mount %s opts=%s", f[4], f[5])
			}
		}
		r.p("mount lines=%d", strings.Count(string(b), "\n"))
	} else {
		r.p("mountinfo err=%v", err)
	}
}

func environment(r *report) {
	env := os.Environ()
	sort.Strings(env)
	r.p("env count=%d", len(env))
	var names []string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		names = append(names, k)
		if sensitive.MatchString(k) {
			r.p("env sensitive %s len=%d shapes=[%s]", k, len(v), shapeCounts(v))
		}
		if strings.HasSuffix(k, "_PROXY") || strings.HasSuffix(k, "_proxy") {
			host := v
			if i := strings.LastIndex(v, "@"); i >= 0 {
				host = "<userinfo>@" + v[i+1:]
			}
			r.p("env proxy %s=%s", k, host)
		}
	}
	r.p("env names=%s", strings.Join(names, ","))
	r.p("env shapes in all values=[%s]", shapeCounts(strings.Join(env, "\n")))
}

func procs(r *report) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		r.p("proc err=%v", err)
		return
	}
	n := 0
	for _, e := range ents {
		pid := e.Name()
		if pid[0] < '0' || pid[0] > '9' {
			continue
		}
		n++
		if n > 300 {
			continue
		}
		comm, _ := os.ReadFile("/proc/" + pid + "/comm")
		line := fmt.Sprintf("pid=%s comm=%q", pid, strings.TrimSpace(string(comm)))
		if b, err := os.ReadFile("/proc/" + pid + "/environ"); err != nil {
			line += fmt.Sprintf(" environ=ERR(%v)", errKind(err))
		} else {
			vars := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
			var hits []string
			for _, kv := range vars {
				k, v, _ := strings.Cut(kv, "=")
				if k == "CLAUDE_CODE_OAUTH_TOKEN" || strings.HasPrefix(k, "ANTHROPIC_") || k == "GITHUB_TOKEN" || k == "GH_TOKEN" || strings.HasPrefix(k, "ACTIONS_") || k == "OVERRIDE_GITHUB_TOKEN" || k == "DEFAULT_WORKFLOW_TOKEN" || k == "ALL_INPUTS" || strings.HasPrefix(k, "INPUT_") {
					hits = append(hits, fmt.Sprintf("%s(len=%d)", k, len(v)))
				}
			}
			line += fmt.Sprintf(" environ=READ vars=%d sensitive=[%s] shapes=[%s]", len(vars), strings.Join(hits, ","), shapeCounts(string(b)))
		}
		if b, err := os.ReadFile("/proc/" + pid + "/cmdline"); err != nil {
			line += fmt.Sprintf(" cmdline=ERR(%v)", errKind(err))
		} else {
			args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
			line += fmt.Sprintf(" argc=%d argv0=%q cmdline_shapes=[%s]", len(args), filepath.Base(args[0]), shapeCounts(string(b)))
		}
		r.p("proc %s", line)
	}
	r.p("proc visible pids=%d", n)
}

func errKind(err error) string {
	switch {
	case err == nil:
		return "ok"
	case os.IsNotExist(err):
		return "not-exist"
	case os.IsPermission(err):
		return "permission"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

func files(r *report) {
	home, _ := os.UserHomeDir()
	roots := []string{
		"/home/runner/.claude", "/home/runner/.claude.json", "/home/runner/.config",
		"/home/runner/work/_temp", "/home/runner/work/_actions",
		"/home/runner/work/_temp/_github_workflow/event.json",
		"/home/runner/work/_temp/_runner_file_commands",
		"/home/runner/runners", "/home/runner/actions-runner", "/opt/actions-runner",
		"/home/runner", home,
	}
	for _, root := range roots {
		walk(r, root)
	}
	for _, k := range []string{"GITHUB_ENV", "GITHUB_OUTPUT", "GITHUB_STATE", "GITHUB_PATH", "GITHUB_EVENT_PATH"} {
		if v := os.Getenv(k); v != "" {
			_, err := os.Stat(v)
			r.p("file env %s set, stat=%s", k, errKind(err))
		} else {
			r.p("file env %s unset", k)
		}
	}
}

func walk(r *report, root string) {
	st, err := os.Stat(root)
	if err != nil {
		r.p("files %s stat=%s", root, errKind(err))
		return
	}
	if !st.IsDir() {
		b, err := os.ReadFile(root)
		r.p("files %s file read=%s size=%d shapes=[%s]", root, errKind(err), len(b), shapeCounts(string(b)))
		return
	}
	var dirs, nfiles, readable, denied int
	var hits []string
	credName := regexp.MustCompile(`(?i)credential|\.runner$|token|secret|oauth|auth\.json`)
	var names []string
	depth0 := strings.Count(root, "/")
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			denied++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			dirs++
			name := d.Name()
			if p != root && (name == "node_modules" || name == "go" || name == ".cache" || name == ".git" || name == "pkg" || name == ".rustup" || name == ".cargo" || name == ".dotnet" || name == ".nvm" || name == "hostedtoolcache" || name == "_tool") {
				return fs.SkipDir
			}
			if strings.Count(p, "/")-depth0 > 6 || dirs > 3000 {
				return fs.SkipDir
			}
			return nil
		}
		nfiles++
		if credName.MatchString(d.Name()) && len(names) < 40 {
			names = append(names, p)
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 || nfiles > 30000 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			denied++
			return nil
		}
		readable++
		if s := shapeCounts(string(b)); s != "" && len(hits) < 60 {
			hits = append(hits, p+"["+s+"]")
		}
		return nil
	})
	r.p("files %s dir dirs=%d files=%d readable=%d errors=%d", root, dirs, nfiles, readable, denied)
	for _, n := range names {
		r.p("files %s credential-like name: %s", root, n)
	}
	for _, h := range hits {
		r.p("files %s secret-shaped content: %s", root, h)
	}
}

func network(r *report) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, "example.com")
	r.p("net dns example.com n=%d err=%v", len(addrs), err)
	for _, a := range []string{"1.1.1.1:443", "example.com:443"} {
		c, err := (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp", a)
		if c != nil {
			_ = c.Close()
		}
		r.p("net direct dial %s err=%v", a, err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	for _, u := range []string{"https://example.com/", "https://proxy.golang.org/github.com/mattn/go-sqlite3/@v/list", "https://api.anthropic.com/", "https://api.github.com/zen"} {
		resp, err := client.Get(u)
		if err != nil {
			r.p("net get %s err=%v", u, err)
			continue
		}
		_ = resp.Body.Close()
		r.p("net get %s status=%d", u, resp.StatusCode)
	}
}

func writes(r *report) {
	home, _ := os.UserHomeDir()
	cache, _ := os.UserCacheDir()
	wd, _ := os.Getwd()
	for _, d := range []string{"/", "/tmp", home, "/home/runner", wd, "/home/runner/work/_temp", "/home/runner/work/_temp/review", "/home/runner/work/_temp/review/scratch", "/home/runner/work/_temp/_runner_file_commands", filepath.Join(cache, "go-build"), "/home/runner/.claude"} {
		f, err := os.CreateTemp(d, "zzprobe-*")
		if err == nil {
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
		}
		r.p("write %s err=%s", d, errKind(err))
	}
	// Appending to the runner's command files would let code set env or
	// outputs for later steps. Test with an empty append only.
	if ents, err := os.ReadDir("/home/runner/work/_temp/_runner_file_commands"); err == nil {
		for _, e := range ents {
			p := "/home/runner/work/_temp/_runner_file_commands/" + e.Name()
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err == nil {
				_ = f.Close()
			}
			r.p("write append-open %s err=%s", e.Name(), errKind(err))
		}
	}
}
