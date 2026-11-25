package render

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Tool is a program a sandboxed build may run, as FindTools found it on the
// server's PATH.
type Tool struct {
	// Name is what the tool is called, such as "ruby" or "ImageMagick".
	Name string
	// Path is where it was found, with every link resolved. Empty when it
	// was not found.
	Path string
	// Required is whether no build can work without it. Without one that is
	// not required, only recipes that use it fail, such as one with a
	// Python transform.
	Required bool
	// Use says what needs the tool, for a message about one that is
	// missing.
	Use string
}

// Tools is what sandboxed builds run, and what the sandbox must show them.
type Tools struct {
	// List is every tool looked for, found or not, in a fixed order.
	List []Tool
	// Read are host paths the sandbox shows read-only even where they lie
	// under a path it covers, such as a Ruby under ~/.rbenv or gems under
	// /var/lib/gems.
	Read []string
	// Path is the PATH a build gets: the directories the tools were found
	// in, and the ones their links lead to.
	Path string
	// Cover are paths inside a Read entry that the sandbox hides again,
	// such as Linuxbrew's var, where its services keep their data.
	Cover []string
	// GemPath is Ruby's gem path as the server's Ruby has it, empty if Ruby
	// was not found. A build gets it as GEM_PATH, since its HOME is the
	// sandbox's own and Ruby would not find a user's gems from there.
	GemPath string
	// GemErr is why the server's Ruby could not say its gem path, nil if it
	// did or Ruby was not found. trmnlp cannot load its gems without it.
	GemErr error
}

// Find returns the path of the tool called name, or "" if it was not
// found.
func (t Tools) Find(name string) string {
	for _, tool := range t.List {
		if tool.Name == name {
			return tool.Path
		}
	}
	return ""
}

// present reports whether every tool found is still where it was found.
func (t Tools) present() bool {
	for _, tool := range t.List {
		if tool.Path == "" {
			continue
		}
		if _, err := os.Stat(tool.Path); err != nil {
			return false
		}
	}
	return true
}

// Missing returns the tools that are required and were not found.
func (t Tools) Missing() []Tool {
	var out []Tool
	for _, tool := range t.List {
		if tool.Required && tool.Path == "" {
			out = append(out, tool)
		}
	}
	return out
}

// toolSpecs are the tools a build runs, by the names they are found under.
// The first name found is used.
var toolSpecs = []struct {
	name     string
	commands []string
	required bool
	use      string
}{
	{"trmnlp", []string{"trmnlp"}, true, "builds every recipe"},
	{"ruby", []string{"ruby"}, true, "runs trmnlp, and recipes' Ruby transforms"},
	{"firefox", []string{"firefox", "firefox-esr"}, true, "draws every recipe"},
	{"geckodriver", []string{"geckodriver"}, true, "lets trmnlp drive Firefox"},
	{"ImageMagick", []string{"magick", "convert"}, true, "turns each page into a PNG"},
	{"Xvfb", []string{"Xvfb"}, true, "gives Firefox a display, without which pages that draw with WebGL, such as maps, draw nothing"},
	{"node", []string{"node"}, false, "runs recipes' JavaScript transforms"},
	{"python", []string{"python3", "python"}, false, "runs recipes' Python transforms"},
	{"php", []string{"php"}, false, "runs recipes' PHP transforms"},
}

// systemDirs hold the system's own programs and libraries. The sandbox
// never covers them, so a tool there needs no Read entry.
var systemDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/opt", "/nix"}

// FindTools looks for trmnlp and the runtimes it starts on pathEnv, the
// server's PATH, and works out what a sandbox must show them. It is not an
// error for a tool to be missing: Tools.Missing names the ones no build
// can do without, and Tools.GemErr says why Ruby could not give its gem
// path. The fontconfig cache is shown too, if there is one, so each build
// does not rebuild it.
func FindTools(ctx context.Context, pathEnv string) Tools {
	var t Tools
	var resolved, found []string
	for _, spec := range toolSpecs {
		tool := Tool{Name: spec.name, Required: spec.required, Use: spec.use}
		for _, name := range spec.commands {
			p := lookPath(name, pathEnv)
			if p == "" {
				continue
			}
			rp, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			tool.Path = rp
			found = append(found, filepath.Dir(p))
			resolved = append(resolved, filepath.Dir(rp))
			entry := readEntry(rp)
			if entry == linuxbrew {
				t.Cover = append(t.Cover, linuxbrew+"/var")
			}
			t.Read = append(t.Read, entry)
			break
		}
		t.List = append(t.List, tool)
	}
	if ruby := t.Find("ruby"); ruby != "" {
		gems, err := gemPath(ctx, ruby)
		t.GemErr = err
		for _, dir := range gems {
			// A gem directory is named by Ruby's version, such as 3.4.0. A
			// wider one, such as a GEM_HOME of a whole home, is not shown.
			if base := filepath.Base(dir); versionLike(base) && strings.Contains(base, ".") || underAny(dir, systemDirs) {
				t.Read = append(t.Read, dir)
			}
		}
		t.GemPath = strings.Join(gems, string(filepath.ListSeparator))
	}
	t.Read = append(t.Read, "/var/cache/fontconfig")
	t.Read = compact(slices.DeleteFunc(t.Read, func(p string) bool { return p == "" || underAny(p, systemDirs) }))
	t.Cover = compact(t.Cover)
	dirs := slices.Concat(resolved, found, []string{"/usr/local/bin", "/usr/bin", "/bin"})
	t.Path = strings.Join(compact(dirs), string(filepath.ListSeparator))
	return t
}

// compact returns paths without duplicates, in their first order.
func compact(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// lookPath finds the executable called name in the directories of pathEnv,
// as a shell does, and returns its path, or "" if there is none. It does
// not read the server's own PATH, so a test can give its own.
func lookPath(name, pathEnv string) string {
	for _, dir := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(dir) {
			// A relative entry would depend on the server's working
			// directory, which a shell would search, and nothing here
			// should.
			continue
		}
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// readEntry returns what a sandbox must show for the tool at rp, a path
// with its links resolved: the install of one version under a version
// manager, such as ~/.rbenv/versions/3.4.1 for a Ruby there, the whole
// Homebrew prefix for one installed by Linuxbrew, and the file alone
// otherwise. A wider entry, such as the directory a binary sits in, could
// show a person's home, which holds their credentials, so a tool installed
// loose under a home gets only its own file, and one that needs its
// neighbors must be installed system-wide or by a version manager.
func readEntry(rp string) string {
	if underAny(rp, systemDirs) {
		return ""
	}
	if underAny(rp, []string{linuxbrew}) {
		return linuxbrew
	}
	parts := strings.Split(rp, string(filepath.Separator))
	// The last "versions" or "installs" with a version after it, within
	// two components: rbenv, pyenv and nodenv's versions/<v>, nvm's
	// versions/node/<v>, and asdf and mise's installs/<tool>/<v>.
	for i := len(parts) - 2; i > 0; i-- {
		if parts[i] != "versions" && parts[i] != "installs" {
			continue
		}
		for j := i + 1; j < len(parts)-1 && j <= i+2; j++ {
			if versionLike(parts[j]) {
				return strings.Join(parts[:j+1], string(filepath.Separator))
			}
		}
	}
	return rp
}

// linuxbrew is Homebrew's prefix on Linux. Its var directory, which
// Tools.Cover hides again, holds the data of the services it runs.
const linuxbrew = "/home/linuxbrew/.linuxbrew"

// versionLike reports whether name looks like a version, such as 3.4.1,
// v22.11.0 or 3.4.0-preview1, which a version manager names an install by.
func versionLike(name string) bool {
	name = strings.TrimPrefix(name, "v")
	return name != "" && name[0] >= '0' && name[0] <= '9'
}

// underAny reports whether p is one of dirs or lies inside one.
func underAny(p string, dirs []string) bool {
	return slices.ContainsFunc(dirs, func(d string) bool { return within(p, d) })
}

// gemPathTimeout bounds asking Ruby for its gem path.
const gemPathTimeout = 10 * time.Second

// gemPath asks ruby for its gem path, as the server's environment sets it.
// Each entry is resolved through links, and one that does not exist is left
// out. A Ruby that cannot answer, or not in time, is an error, since
// trmnlp could not load its gems.
func gemPath(ctx context.Context, ruby string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, gemPathTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ruby, "-e", "puts Gem.path")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ask %s for its gem path: %w", ruby, err)
	}
	var dirs []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		dir := strings.TrimSpace(sc.Text())
		if !filepath.IsAbs(dir) {
			continue
		}
		// One that does not exist, or cannot be resolved, holds nothing a
		// build could load.
		rp, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		dirs = append(dirs, rp)
	}
	return compact(dirs), nil
}

// ToolVersion runs the tool's version command and returns the first line
// of its output. It runs on the host, outside any sandbox, so call it only
// for a tool FindTools found.
func ToolVersion(ctx context.Context, tool Tool) (string, error) {
	if tool.Path == "" {
		return "", fmt.Errorf("%s was not found", tool.Name)
	}
	return version(ctx, tool.Name, tool.Path)
}

// version runs the program at path, the tool called name, with its version
// flag.
func version(ctx context.Context, name, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gemPathTimeout)
	defer cancel()
	// Each tool's version flag is a constant, so no argument comes from
	// anywhere else.
	var cmd *exec.Cmd
	switch name {
	case "trmnlp":
		cmd = exec.CommandContext(ctx, path, "version")
	case "ImageMagick":
		cmd = exec.CommandContext(ctx, path, "-version")
	case "Xvfb":
		// Xvfb has no version flag, and its name says enough.
		return "", nil
	default:
		cmd = exec.CommandContext(ctx, path, "--version")
	}
	out, err := cmd.CombinedOutput()
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if err != nil {
		return line, fmt.Errorf("%s: %w", path, err)
	}
	return line, nil
}
