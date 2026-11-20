//go:build docker

package render

import (
	"archive/tar"
	"bytes"
	"image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// buildRecipe builds a recipe of the given files in the renderer at
// TRMNL_RENDER_URL, and returns its log and PNGs. The renderer refuses a
// build while another runs, so the tests in this file do not run in
// parallel.
func buildRecipe(t *testing.T, files map[string]string) (string, map[string][]byte) {
	t.Helper()
	url := os.Getenv("TRMNL_RENDER_URL")
	if url == "" {
		t.Fatal("set TRMNL_RENDER_URL to a running renderer")
	}
	c := &Client{URL: url, HTTP: &http.Client{Timeout: 3 * time.Minute}}
	got := map[string][]byte{}
	log, err := c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "gray-4"},
		func(tw *tar.Writer) error {
			for name, body := range files {
				if err := AddFile(tw, name, int64(len(body)), strings.NewReader(body)); err != nil {
					return err
				}
			}
			return nil
		},
		func(layout string, r io.Reader) error {
			var buf bytes.Buffer
			_, err := buf.ReadFrom(r)
			got[layout] = buf.Bytes()
			return err
		})
	if err != nil {
		t.Fatalf("Build: %v\n%s", err, log)
	}
	return log, got
}

// TestRendererImage builds a small recipe in a running renderer container.
// Start one, then run:
//
//	docker run --rm --read-only --tmpfs /tmp:mode=1777,size=1g -p 8081:8081 trmnl-byos-render
//	TRMNL_RENDER_URL=http://localhost:8081 go test -tags docker -run TestRendererImage ./internal/render/
//
// The build loads the framework from trmnl.com, so it needs the internet.
func TestRendererImage(t *testing.T) {
	const settings = "strategy: static\nstatic_data: '{\"name\": \"renderer\"}'\nframework_version: latest\nrefresh_interval: 15\ncustom_fields: []\n"
	const view = `<div class="layout layout--col"><span class="title">Hello, {{ name }}</span><span class="description">{{ trmnl.user.time_zone_iana }}</span></div>`
	files := map[string]string{"src/settings.yml": settings, ".trmnlp.yml": "time_zone: America/Los_Angeles\n"}
	for _, layout := range Layouts {
		files["src/"+layout+".liquid"] = view
	}
	_, got := buildRecipe(t, files)
	for _, layout := range Layouts {
		cfg, err := png.DecodeConfig(bytes.NewReader(got[layout]))
		if err != nil || cfg.Width != 800 || cfg.Height != 480 {
			t.Errorf("%s: %dx%d, %v", layout, cfg.Width, cfg.Height, err)
		}
	}
	if err := os.WriteFile(os.TempDir()+"/trmnl-render-full.png", got["full"], 0o600); err != nil {
		t.Fatal(err)
	}
}

// plantTransform, a Python transform, plants a file in every directory the
// renderer's user can write, Python startup code in the user site of the
// build's home and of the account's own, and Node and Ruby startup files in
// the account's home. It prints what it planted, and shows that the
// planted Python code would run.
const plantTransform = `
import os, subprocess, site
planted = []
def plant(path, body=""):
    try:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            f.write(body)
        planted.append(path)
    except OSError:
        pass
for top, dirs, files in os.walk("/"):
    dirs[:] = [d for d in dirs if os.path.join(top, d) not in ("/proc", "/sys")]
    if os.access(top, os.W_OK):
        plant(os.path.join(top, "planted-file"))
code = "print('PLANTED CODE RAN')"
real = os.path.expanduser("~" + os.environ.get("USER", "render"))
for home in (os.environ["HOME"], "/home/render"):
    env = dict(os.environ, HOME=home)
    env.pop("PYTHONNOUSERSITE", None)
    user_site = subprocess.run(["python3", "-m", "site", "--user-site"], env=env, capture_output=True, text=True).stdout.strip()
    plant(os.path.join(user_site, "usercustomize.py"), code)
    print(subprocess.run(["python3", "-c", "0"], env=env, capture_output=True, text=True).stdout.strip(), "in", home)
plant("/home/render/.node_modules/planted.js", "console.log('PLANTED CODE RAN')")
plant("/home/render/.gemrc", "gem: --planted")
print("planted", len(planted), sorted(planted))
`

// checkTransform, a Python transform, starts Python as a build does, and as
// a runtime that ignored the build's home would, and prints every planted
// file it finds.
const checkTransform = `
import os, subprocess
for home in (os.environ["HOME"], "/home/render"):
    env = dict(os.environ, HOME=home)
    env.pop("PYTHONNOUSERSITE", None)
    print(subprocess.run(["python3", "-c", "0"], env=env, capture_output=True, text=True).stdout.strip(), "in", home)
for top, dirs, files in os.walk("/"):
    dirs[:] = [d for d in dirs if os.path.join(top, d) not in ("/proc", "/sys")]
    for f in files:
        if f.startswith("planted") or f in ("usercustomize.py", ".gemrc"):
            print("FOUND", os.path.join(top, f))
`

// TestRendererLeavesNothing checks, in a running renderer container, that
// what a recipe's transform plants wherever it can write is gone before the
// next build, which neither runs nor finds it. Start the renderer as the
// Compose file does, with a read-only root file system:
//
//	docker run --rm --read-only --tmpfs /tmp:mode=1777,size=1g -p 8081:8081 trmnl-byos-render
//	TRMNL_RENDER_URL=http://localhost:8081 go test -tags docker -run TestRendererLeavesNothing ./internal/render/
func TestRendererLeavesNothing(t *testing.T) {
	const settings = "strategy: static\nstatic_data: '{\"name\": \"renderer\"}'\nframework_version: latest\nrefresh_interval: 15\ncustom_fields: []\n"
	recipe := func(transform string) map[string]string {
		files := map[string]string{"src/settings.yml": settings, "src/transform.py": transform, ".trmnlp.yml": "time_zone: UTC\n"}
		for _, layout := range Layouts {
			files["src/"+layout+".liquid"] = "{{ name }}"
		}
		return files
	}
	log, _ := buildRecipe(t, recipe(plantTransform))
	t.Logf("plant build:\n%s", log)
	if !strings.Contains(log, "PLANTED CODE RAN in") || !strings.Contains(log, "planted ") {
		t.Fatal("the first build planted nothing that would run")
	}
	log, _ = buildRecipe(t, recipe(checkTransform))
	t.Logf("check build:\n%s", log)
	if strings.Contains(log, "PLANTED CODE RAN") || strings.Contains(log, "FOUND") {
		t.Error("the second build ran or found what the first planted")
	}
}

// ghostTransform, a Python transform, starts a process in a session of its
// own whose main thread exits while another thread runs, so it shows the
// state Z, as a zombie does, but lives on. It waits until it does.
const ghostTransform = `
import subprocess, time
ghost = "import ctypes, threading, time\nthreading.Thread(target=lambda: time.sleep(3600)).start()\ntime.sleep(0.2)\nctypes.CDLL(None).pthread_exit(None)\n"
p = subprocess.Popen(["python3", "-c", ghost, "ghost-marker"], start_new_session=True,
    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
ready = False
for _ in range(100):
    try:
        status = open("/proc/%d/status" % p.pid).read()
    except OSError:
        break
    if "State:\tZ" in status and "Threads:\t1\n" not in status:
        ready = True
        break
    time.sleep(0.05)
print("GHOST READY" if ready else "GHOST NOT READY", p.pid)
`

// ghostCheckTransform, a Python transform, prints whether a ghost is alive.
const ghostCheckTransform = `
import os
alive = []
for pid in os.listdir("/proc"):
    try:
        if b"ghost-marker" in open("/proc/%s/cmdline" % pid, "rb").read():
            alive.append(pid)
    except OSError:
        pass
print("GHOST ALIVE" if alive else "NO GHOST", alive)
`

// TestRendererKillsGhosts checks, in a running renderer container, that the
// sweep kills a process whose main thread exited, which looks like a
// zombie, so it cannot see the next build. Start the renderer as for
// TestRendererLeavesNothing.
func TestRendererKillsGhosts(t *testing.T) {
	const settings = "strategy: static\nstatic_data: '{\"name\": \"renderer\"}'\nframework_version: latest\nrefresh_interval: 15\ncustom_fields: []\n"
	recipe := func(transform string) map[string]string {
		files := map[string]string{"src/settings.yml": settings, "src/transform.py": transform, ".trmnlp.yml": "time_zone: UTC\n"}
		for _, layout := range Layouts {
			files["src/"+layout+".liquid"] = "{{ name }}"
		}
		return files
	}
	log, _ := buildRecipe(t, recipe(ghostTransform))
	t.Logf("ghost build:\n%s", log)
	if !strings.Contains(log, "GHOST READY") || strings.Contains(log, "GHOST NOT READY") {
		t.Fatal("the first build made no ghost")
	}
	log, _ = buildRecipe(t, recipe(ghostCheckTransform))
	t.Logf("check build:\n%s", log)
	if !strings.Contains(log, "NO GHOST") || strings.Contains(log, "GHOST ALIVE") {
		t.Error("a ghost outlived its build")
	}
}

// selfPostTransform, a Python transform, sends the shim a build request of
// its own, as a recipe could to come back after its sweep, and prints the
// status it gets.
const selfPostTransform = `
import urllib.request, urllib.error
req = urllib.request.Request("http://127.0.0.1:8081/build", data=b"x" * 1024, method="POST",
    headers={"Content-Type": "application/x-tar"})
try:
    status = urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=10).status
except urllib.error.HTTPError as e:
    status = e.code
except Exception as e:
    status = repr(e)
print("SELF POST", status)
`

// TestRendererRefusesOwnBuilds checks, in a running renderer container,
// that a build request a build sends to the shim is refused at once, so
// it cannot wait out the build's sweep and run after it. Start the renderer
// as for TestRendererLeavesNothing.
func TestRendererRefusesOwnBuilds(t *testing.T) {
	const settings = "strategy: static\nstatic_data: '{\"name\": \"renderer\"}'\nframework_version: latest\nrefresh_interval: 15\ncustom_fields: []\n"
	files := map[string]string{"src/settings.yml": settings, "src/transform.py": selfPostTransform, ".trmnlp.yml": "time_zone: UTC\n"}
	for _, layout := range Layouts {
		files["src/"+layout+".liquid"] = "{{ name }}"
	}
	log, _ := buildRecipe(t, files)
	t.Logf("build:\n%s", log)
	if !strings.Contains(log, "SELF POST 503") || strings.Contains(log, "SELF POST 200") {
		t.Error("the shim did not refuse a build's own request")
	}
}
