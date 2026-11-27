package render

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestReadEntry checks what a sandbox is told to show for a tool: nothing
// for a system path, the install of one version under a version manager,
// the Linuxbrew prefix, and otherwise only the file itself, never the
// directory around it, which could be a person's home.
func TestReadEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path, want string
	}{
		{"system bin", "/usr/bin/ruby", ""},
		{"opt", "/opt/x/bin/y", ""},
		{"rbenv", "/home/u/.rbenv/versions/3.4.1/bin/ruby", "/home/u/.rbenv/versions/3.4.1"},
		{"nvm", "/home/u/.nvm/versions/node/v22.11.0/bin/node", "/home/u/.nvm/versions/node/v22.11.0"},
		{"asdf", "/home/u/.asdf/installs/ruby/3.4.1/bin/ruby", "/home/u/.asdf/installs/ruby/3.4.1"},
		{"mise", "/home/u/.local/share/mise/installs/node/22/bin/node", "/home/u/.local/share/mise/installs/node/22"},
		{"linuxbrew", "/home/linuxbrew/.linuxbrew/Cellar/x/1/bin/x", "/home/linuxbrew/.linuxbrew"},
		{"loose in home", "/home/u/bin/trmnlp", "/home/u/bin/trmnlp"},
		{"loose in local bin", "/home/u/.local/bin/trmnlp", "/home/u/.local/bin/trmnlp"},
		{"system gems", "/var/lib/gems/3.3.0/bin/trmnlp", "/var/lib/gems/3.3.0/bin/trmnlp"},
		{"versions with no version after it", "/home/u/.rbenv/versions/shims/ruby", "/home/u/.rbenv/versions/shims/ruby"},
		{"version too far below versions", "/home/u/versions/a/b/3.0/bin/tool", "/home/u/versions/a/b/3.0/bin/tool"},
		{"the file is the version", "/home/u/versions/3.4.1", "/home/u/versions/3.4.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := readEntry(tc.path); got != tc.want {
				t.Errorf("readEntry(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestVersionLike checks which directory names a version manager's installs
// are told apart by.
func TestVersionLike(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"3.4.1", true},
		{"v22.11.0", true},
		{"3.4.0-preview1", true},
		{"22", true},
		{"", false},
		{"v", false},
		{"node", false},
		{"latest", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := versionLike(tc.name); got != tc.want {
				t.Errorf("versionLike(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// fakeExecutables writes an executable shell script called each of names in
// dir, and returns dir with its links resolved.
func fakeExecutables(t *testing.T, dir string, names ...string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		//nolint:gosec // G306: a fake tool must be executable.
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// toolNames returns the names of tools.
func toolNames(tools []Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name)
	}
	return out
}

// TestFindTools checks FindTools against a PATH of fake tools: that it finds
// each in the fixed order, resolves links, and works out the PATH, gem path
// and read-only paths a sandbox needs from them.
func TestFindTools(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	realDir := filepath.Join(root, "real")
	gems := filepath.Join(root, "gems")
	for _, d := range []string{bin, realDir, filepath.Join(gems, "3.4.1"), filepath.Join(gems, "plain")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	fakeExecutables(t, bin, "trmnlp", "firefox-esr", "Xvfb")
	fakeExecutables(t, realDir, "convert")
	// The fake ruby answers Gem.path with a version directory, one that is
	// not named for a version, one that does not exist, and a relative one.
	script := "#!/bin/sh\nprintf '%s\\n' '" + filepath.Join(gems, "3.4.1") + "' '" + filepath.Join(gems, "plain") + "' '" + filepath.Join(gems, "missing") + "' relative/gems\n"
	//nolint:gosec // G306: a fake ruby must be executable.
	if err := os.WriteFile(filepath.Join(bin, "ruby"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// convert is a link to a file elsewhere, and geckodriver is a link to
	// another tool, so two tools resolve to the same file.
	if err := os.Symlink(filepath.Join(realDir, "convert"), filepath.Join(bin, "convert")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(bin, "trmnlp"), filepath.Join(bin, "geckodriver")); err != nil {
		t.Fatal(err)
	}

	tools := FindTools(t.Context(), bin)

	wantNames := []string{"trmnlp", "ruby", "firefox", "geckodriver", "ImageMagick", "Xvfb", "node", "python", "php"}
	if got := toolNames(tools.List); !slices.Equal(got, wantNames) {
		t.Fatalf("tools = %v, want %v", got, wantNames)
	}
	for name, want := range map[string]string{
		"trmnlp":      filepath.Join(bin, "trmnlp"),
		"ruby":        filepath.Join(bin, "ruby"),
		"firefox":     filepath.Join(bin, "firefox-esr"),
		"geckodriver": filepath.Join(bin, "trmnlp"),
		"ImageMagick": filepath.Join(realDir, "convert"),
		"Xvfb":        filepath.Join(bin, "Xvfb"),
		"node":        "",
		"python":      "",
		"php":         "",
	} {
		if got := tools.Find(name); got != want {
			t.Errorf("Find(%q) = %q, want %q", name, got, want)
		}
	}
	if got := tools.Find("nothing"); got != "" {
		t.Errorf("Find of an unknown tool = %q", got)
	}
	if m := tools.Missing(); len(m) != 0 {
		t.Errorf("Missing() = %v, want none: node, python and php are optional", toolNames(m))
	}
	wantPath := strings.Join([]string{bin, realDir, "/usr/local/bin", "/usr/bin", "/bin"}, ":")
	if tools.Path != wantPath {
		t.Errorf("Path = %q, want %q", tools.Path, wantPath)
	}
	wantGems := filepath.Join(gems, "3.4.1") + ":" + filepath.Join(gems, "plain")
	if tools.GemPath != wantGems {
		t.Errorf("GemPath = %q, want %q", tools.GemPath, wantGems)
	}
	wantRead := []string{
		filepath.Join(bin, "trmnlp"), filepath.Join(bin, "ruby"), filepath.Join(bin, "firefox-esr"),
		filepath.Join(realDir, "convert"), filepath.Join(bin, "Xvfb"),
		filepath.Join(gems, "3.4.1"), filepath.Join(gems, "plain"), "/var/cache/fontconfig",
	}
	if !slices.Equal(tools.Read, wantRead) {
		t.Errorf("Read = %v, want %v", tools.Read, wantRead)
	}
}

// TestFindToolsMissing checks which tools are named as missing, and which
// alternative names are found, for a PATH with only some tools on it. It
// runs no ruby, so no tool here is one.
func TestFindToolsMissing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		files []string
		// find maps a tool's name to the file it is expected at, "" for none.
		find map[string]string
		// missing is the names Missing returns.
		missing []string
	}{
		{
			name:    "nothing",
			missing: []string{"trmnlp", "ruby", "firefox", "geckodriver", "ImageMagick", "Xvfb"},
		},
		{
			name:    "firefox-esr when there is no firefox",
			files:   []string{"firefox-esr"},
			find:    map[string]string{"firefox": "firefox-esr"},
			missing: []string{"trmnlp", "ruby", "geckodriver", "ImageMagick", "Xvfb"},
		},
		{
			name:    "firefox before firefox-esr",
			files:   []string{"firefox-esr", "firefox"},
			find:    map[string]string{"firefox": "firefox"},
			missing: []string{"trmnlp", "ruby", "geckodriver", "ImageMagick", "Xvfb"},
		},
		{
			name:    "convert when there is no magick",
			files:   []string{"convert"},
			find:    map[string]string{"ImageMagick": "convert"},
			missing: []string{"trmnlp", "ruby", "firefox", "geckodriver", "Xvfb"},
		},
		{
			name:    "magick before convert",
			files:   []string{"convert", "magick"},
			find:    map[string]string{"ImageMagick": "magick"},
			missing: []string{"trmnlp", "ruby", "firefox", "geckodriver", "Xvfb"},
		},
		{
			name:    "optional tools found",
			files:   []string{"node", "python3", "php"},
			find:    map[string]string{"node": "node", "python": "python3", "php": "php"},
			missing: []string{"trmnlp", "ruby", "firefox", "geckodriver", "ImageMagick", "Xvfb"},
		},
		{
			name:    "python when there is no python3",
			files:   []string{"python"},
			find:    map[string]string{"python": "python"},
			missing: []string{"trmnlp", "ruby", "firefox", "geckodriver", "ImageMagick", "Xvfb"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin := fakeExecutables(t, t.TempDir(), tc.files...)
			tools := FindTools(t.Context(), bin)
			for name, file := range tc.find {
				want := filepath.Join(bin, file)
				if got := tools.Find(name); got != want {
					t.Errorf("Find(%q) = %q, want %q", name, got, want)
				}
			}
			if got := toolNames(tools.Missing()); !slices.Equal(got, tc.missing) {
				t.Errorf("Missing() = %v, want %v", got, tc.missing)
			}
			if tools.GemPath != "" {
				t.Errorf("GemPath = %q with no ruby", tools.GemPath)
			}
		})
	}
}

// TestFindToolsIgnoresUnusable checks that a PATH entry that is relative, a
// file that is not executable, and a directory named like a tool are not
// found, since a shell's PATH would not use the first and exec cannot run
// the others, and that a later usable one is.
func TestFindToolsIgnoresUnusable(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(root, "plain")
	good := filepath.Join(root, "good")
	for _, d := range []string{plain, good, filepath.Join(plain, "Xvfb")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(plain, "trmnlp"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeExecutables(t, good, "geckodriver")
	rel, err := filepath.Rel(mustGetwd(t), good)
	if err != nil {
		t.Skipf("no relative path from the working directory: %v", err)
	}
	for _, pathEnv := range []string{rel, plain, "", rel + ":" + plain + ":" + good} {
		tools := FindTools(t.Context(), pathEnv)
		for _, name := range []string{"trmnlp", "Xvfb"} {
			if got := tools.Find(name); got != "" {
				t.Errorf("PATH %q: Find(%q) = %q, want none", pathEnv, name, got)
			}
		}
	}
	if got, want := FindTools(t.Context(), rel+":"+plain+":"+good).Find("geckodriver"), filepath.Join(good, "geckodriver"); got != want {
		t.Errorf("geckodriver after a relative entry = %q, want %q", got, want)
	}
	if got := FindTools(t.Context(), rel).Find("geckodriver"); got != "" {
		t.Errorf("geckodriver on a relative PATH only = %q, want none", got)
	}
}

// mustGetwd returns the working directory.
func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// TestFindToolsGemDirs checks that a sandbox is told to show every gem
// directory that neither is nor holds a person's home, whatever its name,
// such as a GEM_HOME of ~/gems, since trmnlp fails in every build without
// its gems. Each stays in GemPath.
func TestFindToolsGemDirs(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o750); err != nil {
		t.Fatal(err)
	}
	names := []string{"3.4.0", "3.5.0-preview1", "22", "2fa", "x.1", "plain"}
	args := make([]string, 0, len(names))
	wantGems := make([]string, 0, len(names))
	for _, name := range names {
		dir := filepath.Join(root, "gems", name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		args = append(args, "'"+dir+"'")
		wantGems = append(wantGems, dir)
	}
	script := "#!/bin/sh\nprintf '%s\\n' " + strings.Join(args, " ") + "\n"
	//nolint:gosec // G306: a fake ruby must be executable.
	if err := os.WriteFile(filepath.Join(bin, "ruby"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	tools := FindTools(t.Context(), bin)

	if tools.GemErr != nil {
		t.Fatalf("GemErr = %v", tools.GemErr)
	}
	if want := strings.Join(wantGems, ":"); tools.GemPath != want {
		t.Errorf("GemPath = %q, want %q", tools.GemPath, want)
	}
	for _, name := range names {
		dir := filepath.Join(root, "gems", name)
		if !slices.Contains(tools.Read, dir) {
			t.Errorf("gem dir %q is not in Read = %v", name, tools.Read)
		}
	}
	if len(tools.HiddenGems) != 0 {
		t.Errorf("HiddenGems = %v, want none", tools.HiddenGems)
	}
}

// TestHoldsHome checks which gem directories are kept from builds because
// they are, or hold, a person's home, where credentials are: / and the
// directories of homes, each home, and the server's own home and what
// holds it, but not a directory inside a home.
func TestHoldsHome(t *testing.T) {
	t.Parallel()
	// As on Fedora Atomic, where /home is a link to /var/home, so a gem
	// directory's real path is under /var/home.
	view := homeView{parents: []string{"/home", "/var/home"}, homes: []string{"/root", "/srv/trmnl"}}
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{"/", true},
		{"/home", true},
		{"/root", true},
		{"/home/pat", true},
		{"/var/home", true},
		{"/var/home/pat", true},
		{"/var", true},
		{"/srv", true},
		{"/srv/trmnl", true},
		{"/home/pat/gems", false},
		{"/home/pat/.local/share/gem/ruby/3.4.0", false},
		{"/srv/trmnl/gems", false},
		{"/var/lib/gems/3.3.0", false},
		{"/usr/local/bundle", false},
	} {
		if got := view.holdsHome(tc.dir); got != tc.want {
			t.Errorf("holdsHome(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
	if v := hostHomes(""); v.holdsHome("/srv") {
		t.Error("holdsHome counted a home that is unknown")
	}
	if v := hostHomes("relative"); v.holdsHome("/srv") {
		t.Error("holdsHome counted a home that is relative")
	}
	// An account with no home has a HOME of /, which is no home, so the
	// system's own gem directories are still shown.
	for _, dir := range []string{"/usr/local/bundle", "/usr/share/rubygems-integration/all"} {
		if v := hostHomes("/"); !v.gemShown(dir) {
			t.Errorf("with a home of /, gemShown(%q) = false", dir)
		}
	}
}

// TestFindToolsGemErr checks that a ruby that cannot give its gem path is
// reported in GemErr with no gem directories, since trmnlp could not load
// its gems, and that one that can leaves GemErr nil.
func TestFindToolsGemErr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		script string
		wantOK bool
	}{
		{"ruby exits 1", "#!/bin/sh\nexit 1\n", false},
		{"ruby prints dirs and succeeds", "#!/bin/sh\necho /nonexistent/gems/3.4.0\n", true},
		{"ruby prints nothing and succeeds", "#!/bin/sh\nexit 0\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin := fakeExecutables(t, t.TempDir())
			//nolint:gosec // G306: a fake ruby must be executable.
			if err := os.WriteFile(filepath.Join(bin, "ruby"), []byte(tc.script), 0o755); err != nil {
				t.Fatal(err)
			}
			tools := FindTools(t.Context(), bin)
			if tools.Find("ruby") == "" {
				t.Fatal("ruby was not found")
			}
			if gotOK := tools.GemErr == nil; gotOK != tc.wantOK {
				t.Errorf("GemErr = %v, want nil = %v", tools.GemErr, tc.wantOK)
			}
			if tools.GemErr != nil && !strings.Contains(tools.GemErr.Error(), "gem path") {
				t.Errorf("GemErr = %q, want it to mention the gem path", tools.GemErr)
			}
			// A fake ruby's gem dirs do not exist, so there are none either way.
			if tools.GemPath != "" {
				t.Errorf("GemPath = %q, want none", tools.GemPath)
			}
		})
	}
	if got := FindTools(t.Context(), "/nonexistent").GemErr; got != nil {
		t.Errorf("GemErr = %v with no ruby, want nil", got)
	}
}

// TestToolsPresent checks that present is true while every tool found is
// still on disk, false once one is removed, and ignores tools not found, so
// a sandbox looks for its tools again when an upgrade removes one.
func TestToolsPresent(t *testing.T) {
	t.Parallel()
	bin := fakeExecutables(t, t.TempDir(), "trmnlp", "Xvfb")
	tools := FindTools(t.Context(), bin)
	if tools.Find("trmnlp") == "" || tools.Find("Xvfb") == "" {
		t.Fatalf("tools not found: %v", tools.List)
	}
	if !tools.present() {
		t.Error("present() = false with every tool in place")
	}
	if !(Tools{}).present() {
		t.Error("present() = false for no tools")
	}
	if err := os.Remove(filepath.Join(bin, "Xvfb")); err != nil {
		t.Fatal(err)
	}
	if tools.present() {
		t.Error("present() = true after a tool was removed")
	}
}

// TestFindToolsHiddenGems checks that a gem directory that is the server's
// own home is kept from builds, and reported in HiddenGems, rather than
// shown, which would show the home's credentials.
func TestFindToolsHiddenGems(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("this process has no home to put in a gem path: %v", err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Skipf("this process's home cannot be resolved: %v", err)
	}
	bin := t.TempDir()
	//nolint:gosec // G306: a fake ruby must be executable.
	if err := os.WriteFile(filepath.Join(bin, "ruby"), []byte("#!/bin/sh\nprintf '%s\\n' '"+home+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tools := FindTools(t.Context(), bin)
	if !slices.Equal(tools.HiddenGems, []string{home}) {
		t.Errorf("HiddenGems = %v, want [%s]", tools.HiddenGems, home)
	}
	if slices.Contains(tools.Read, home) {
		t.Errorf("Read = %v shows the home", tools.Read)
	}
}

// TestGemShown checks which gem directories builds may see. A directory in
// a person's home is shown only when it is named by a Ruby version, since
// ~/.gem holds the RubyGems push key in credentials, and a GEM_HOME such as
// ~/.local or ~/gems could hold anything of theirs. A home, or what holds
// one, never is. Anywhere else, any directory is.
func TestGemShown(t *testing.T) {
	t.Parallel()
	// As on Fedora Atomic, where /home is a link to /var/home, so a gem
	// directory's real path is under /var/home.
	view := homeView{parents: []string{"/home", "/var/home"}, homes: []string{"/root", "/srv/trmnl"}}
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{"/home/pat/.gem", false},
		{"/var/home/pat/.gem", false},
		{"/var/home/pat/.local", false},
		{"/var/home/pat/.gem/ruby/3.4.0", true},
		{"/home/pat/.local", false},
		{"/home/pat/gems", false},
		{"/root/.gem", false},
		{"/srv/trmnl/.gem", false},
		{"/home/pat", false},
		{"/srv", false},
		{"/home/pat/.gem/ruby/3.4.0", true},
		{"/home/pat/.local/share/gem/ruby/3.4.0", true},
		{"/home/pat/.rbenv/versions/3.4.1/lib/ruby/gems/3.4.0", true},
		{"/srv/trmnl/.gem/ruby/3.4.0", true},
		{"/home/pat/.gem/ruby/22", false},
		{"/var/lib/gems/3.3.0", true},
		{"/usr/local/bundle", true},
		{"/opt/gems", true},
	} {
		if got := view.gemShown(tc.dir); got != tc.want {
			t.Errorf("gemShown(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}

// TestNewHomeViewResolvesLinks checks that a home reached through a link is
// known by its real path too, since a gem directory is compared by its real
// path, and a home missed that way would be shown whole.
func TestNewHomeViewResolvesLinks(t *testing.T) {
	t.Parallel()
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"homes/pat/.gem", "me/.local"} {
		if err := os.MkdirAll(filepath.Join(target, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	links := t.TempDir()
	for _, l := range []string{"homes", "me"} {
		if err := os.Symlink(filepath.Join(target, l), filepath.Join(links, l)); err != nil {
			t.Fatal(err)
		}
	}
	view := newHomeView([]string{filepath.Join(links, "homes")}, []string{filepath.Join(links, "me")})
	for _, dir := range []string{filepath.Join(target, "homes", "pat", ".gem"), filepath.Join(target, "me", ".local"), filepath.Join(target, "homes", "pat")} {
		if view.gemShown(dir) {
			t.Errorf("gemShown(%q) = true, though it is in a home reached through a link", dir)
		}
	}
}
