package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// cliFixture is one CLI test environment. The default flavour is a synthetic
// repository root with private homes below Root. withCheckoutConfig instead
// makes Root a fixture-owned copy of the checkout's shipped trees, with
// private homes in a temporary directory. The homes are fixed when the fixture
// is built; a test that needs another home passes Options.Environment.
type cliFixture struct {
	t           *testing.T
	Root        string
	Home        string
	ConfigHome  string
	DataHome    string
	Bin         string
	WorkingDir  string
	Environment []string
	owned       []string
	exec        *testkit.ExecFake
}

// fixtureOption configures newFixture. Options apply in the order given,
// after the flavour's base setup.
type fixtureOption struct {
	checkout bool
	apply    func(*cliFixture)
}

// checkoutTrees are the checkout's tracked trees that withCheckoutConfig copies.
var checkoutTrees = []string{"bin", "completions", "config", "scripts"}

// fixtureEnvironment names the settings that place the fixture's homes, shipped
// config and tools; withEnv and withoutEnv refuse them.
var fixtureEnvironment = []string{"HOME", "SUBYARD_OPERATOR_HOME", "SUBYARD_CONFIG_HOME", "XDG_CONFIG_HOME",
	"SUBYARD_HOME", "SUBYARD_CONFIG_DIR", "PATH"}

// withCheckoutConfig makes Root a fixture-owned copy of the checkout's tracked
// bin/, completions/, config/ and scripts/ files, without links, .git or
// private/, and loads the shipped config from that copy. A test may seed
// private/ below Root. Tests that read other checkout files for reference
// use repositoryRoot and must not write there.
func withCheckoutConfig() fixtureOption { return fixtureOption{checkout: true} }

// withWorkingDir sets the WorkingDir that New uses when Options leave it
// empty; an empty path keeps it empty.
func withWorkingDir(path string) fixtureOption {
	return fixtureOption{apply: func(fixture *cliFixture) { fixture.WorkingDir = path }}
}

// withCommands installs an ExecFake shim for each tool. A bare name goes to Bin,
// which is put first on PATH for the test process and in Environment; a name
// with a slash is a local path below Root, such as a repository script. The
// fake's unexpected-call check runs at cleanup, and cleanups run in reverse
// order: anything that can still call a tool when its own cleanup runs must be
// registered after the fixture.
func withCommands(tools ...string) fixtureOption {
	return fixtureOption{apply: func(fixture *cliFixture) {
		fixture.t.Helper()
		if fixture.exec == nil {
			fixture.exec = testkit.NewExecFake(fixture.t, fixture.Bin)
		}
		onPath := false
		for _, tool := range tools {
			if !strings.ContainsRune(tool, '/') {
				fixture.exec.Install(tool, filepath.Join(fixture.Bin, tool))
				onPath = true
				continue
			}
			if !localCommandPath(tool) {
				fixture.t.Fatalf("fixture command %q is not a clean path below Root", tool)
			}
			fixture.exec.Install(tool, filepath.Join(fixture.Root, tool))
		}
		if onPath && !slices.Contains(filepath.SplitList(os.Getenv("PATH")), fixture.Bin) {
			fixture.t.Setenv("PATH", fixture.Bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			fixture.Environment = slices.Clip(append(fixture.Environment, "PATH="+os.Getenv("PATH")))
		}
	}}
}

// localCommandPath reports whether tool is a clean relative path that stays below Root.
func localCommandPath(tool string) bool {
	return filepath.IsLocal(tool) && filepath.Clean(tool) == tool
}

// refuseFixtureEnvironment fails the test when name is one the fixture places itself.
func refuseFixtureEnvironment(t *testing.T, name string) {
	t.Helper()
	if fixedSetting(name) {
		t.Fatalf("fixture environment %s is fixed by the fixture; pass Options.Environment at the call site", name)
	}
}

func fixedSetting(name string) bool { return slices.Contains(fixtureEnvironment, name) }

// withEnv appends NAME=value assignments; the last assignment of a name wins.
func withEnv(assignments ...string) fixtureOption {
	return fixtureOption{apply: func(fixture *cliFixture) {
		fixture.t.Helper()
		for _, assignment := range assignments {
			name, _, ok := strings.Cut(assignment, "=")
			if !ok || name == "" {
				fixture.t.Fatalf("fixture environment %q is not NAME=value", assignment)
			}
			refuseFixtureEnvironment(fixture.t, name)
		}
		fixture.Environment = slices.Clip(append(fixture.Environment, assignments...))
	}}
}

// withoutEnv removes every assignment of the names.
func withoutEnv(names ...string) fixtureOption {
	return fixtureOption{apply: func(fixture *cliFixture) {
		fixture.t.Helper()
		for _, name := range names {
			refuseFixtureEnvironment(fixture.t, name)
		}
		fixture.Environment = slices.Clip(slices.DeleteFunc(fixture.Environment, func(assignment string) bool {
			name, _, _ := strings.Cut(assignment, "=")
			return slices.Contains(names, name)
		}))
	}}
}

// withAudit enables the audit log that the base environment disables.
func withAudit() fixtureOption { return withoutEnv("SUBYARD_NO_AUDIT") }

// withYards creates the private registration directory of each named yard.
func withYards(names ...string) fixtureOption {
	return fixtureOption{apply: func(fixture *cliFixture) {
		fixture.t.Helper()
		for _, name := range names {
			if !localCommandPath(name) || strings.ContainsRune(name, '/') {
				fixture.t.Fatalf("fixture yard %q is not a plain name", name)
			}
			if err := os.MkdirAll(filepath.Join(fixture.ConfigHome, "yards", name), 0o700); err != nil {
				fixture.t.Fatal(err)
			}
		}
	}}
}

func newFixture(t *testing.T, options ...fixtureOption) *cliFixture {
	t.Helper()
	fixture := &cliFixture{t: t}
	if slices.ContainsFunc(options, func(option fixtureOption) bool { return option.checkout }) {
		_, home, _, environment := configCommandFixture(t)
		private := filepath.Dir(home)
		if err := os.Chmod(private, 0o700); err != nil {
			t.Fatal(err)
		}
		// configCommandFixture links source/config to the checkout; the copy replaces it.
		link := filepath.Join(private, "source", "config")
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("configCommandFixture layout changed: %v %v", info, err)
		}
		if err := errors.Join(os.Remove(link), os.Remove(filepath.Dir(link))); err != nil {
			t.Fatal(err)
		}
		fixture.Root = testkit.TempDir(t)
		copyCheckoutTrees(t, fixture.Root)
		fixture.Environment = append(slices.DeleteFunc(environment, func(assignment string) bool {
			return strings.HasPrefix(assignment, "SUBYARD_CONFIG_DIR=")
		}), "SUBYARD_CONFIG_DIR="+filepath.Join(fixture.Root, "config"))
		fixture.owned = []string{fixture.Root, private}
	} else {
		root, environment, _ := nativeFixture(t)
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
		fixture.Root, fixture.Environment, fixture.owned = root, environment, []string{root}
	}
	fixture.Environment = slices.Clip(fixture.Environment)
	fixture.Home, fixture.ConfigHome, fixture.DataHome = fixture.Env("HOME"), fixture.Env("SUBYARD_CONFIG_HOME"), fixture.Env("SUBYARD_HOME")
	for _, home := range []string{fixture.Home, fixture.ConfigHome, fixture.DataHome} {
		if !filepath.IsAbs(home) || !slices.ContainsFunc(fixture.owned, func(owned string) bool { return below(owned, home) }) {
			t.Fatalf("fixture home %q is not below the fixture's directories", home)
		}
	}
	fixture.WorkingDir = fixture.Root
	fixture.Bin = testkit.TempDir(t)
	fixture.owned = append(fixture.owned, fixture.Bin)
	for _, option := range options {
		if option.apply != nil {
			option.apply(fixture)
		}
	}
	return fixture
}

var checkoutFiles = sync.OnceValues(func() ([]string, error) { return trackedCheckoutFiles(checkoutSource()) })

func checkoutSource() string {
	_, source, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}

// trackedCheckoutFiles lists the checkout trees' tracked files relative to
// source, which may be below its Git top level. Only an export, which Git
// reports is not in a repository and has no .git of its own, falls back to
// every file below the trees; any other Git failure is an error.
func trackedCheckoutFiles(source string) ([]string, error) {
	// The export decision reads Git's message, so it runs in the C locale.
	environment := append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C", "LANGUAGE=")
	var stderr bytes.Buffer
	top := exec.Command("git", "-C", source, "rev-parse", "--show-toplevel")
	top.Env, top.Stderr = environment, &stderr
	if _, err := top.Output(); err == nil {
		list := exec.Command("git", append([]string{"-C", source, "ls-files", "-z", "--"}, checkoutTrees...)...)
		list.Env = environment
		listed, err := list.Output()
		if err != nil {
			return nil, fmt.Errorf("list checkout files in %s: %w", source, err)
		}
		return strings.Split(strings.TrimSuffix(string(listed), "\x00"), "\x00"), nil
	} else if _, statErr := os.Lstat(filepath.Join(source, ".git")); !strings.Contains(stderr.String(), "not a git repository") ||
		!errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("list checkout files in %s: %w: %s", source, err, strings.TrimSpace(stderr.String()))
	}
	var files []string
	for _, tree := range checkoutTrees {
		err := filepath.WalkDir(filepath.Join(source, tree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			relative, err := filepath.Rel(source, path)
			files = append(files, relative)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// copyCheckoutTrees copies the checkout trees' regular files with their modes
// to root; a tracked file missing from the worktree is skipped.
func copyCheckoutTrees(t *testing.T, root string) {
	t.Helper()
	source := checkoutSource()
	files, err := checkoutFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range files {
		info, err := os.Lstat(filepath.Join(source, relative))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("unsupported checkout entry %s: %v", relative, info.Mode())
		}
		data, err := os.ReadFile(filepath.Join(source, relative))
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, destination, data, info.Mode().Perm())
	}
}

// below reports whether path is lexically directory or below it.
func below(directory, path string) bool {
	relative, err := filepath.Rel(directory, filepath.Clean(path))
	return err == nil && (relative == "." || filepath.IsLocal(relative))
}

// ownedPath resolves a fixture path and the fixture directory that holds it:
// a local path is below Root; an absolute path must be lexically below one of
// the fixture's own directories.
func (fixture *cliFixture) ownedPath(path string) (string, string, error) {
	if !filepath.IsAbs(path) {
		if !filepath.IsLocal(path) {
			return "", "", fmt.Errorf("fixture path %q is not a local path below Root", path)
		}
		return fixture.Root, filepath.Join(fixture.Root, path), nil
	}
	for _, owned := range fixture.owned {
		if below(owned, path) {
			return owned, filepath.Clean(path), nil
		}
	}
	return "", "", fmt.Errorf("fixture path %s is outside the fixture's directories", path)
}

// writeContained writes path below owned without following links: every
// parent below owned must be a real directory or is created as one, and the
// file is opened with O_NOFOLLOW. This refuses links that exist when the call
// starts. It does not stop a parent being swapped for a link between its Lstat
// and the open: the fixture tree belongs to the test, which does not race its
// own writes.
func writeContained(owned, path string, data []byte, mode os.FileMode) error {
	relative, err := filepath.Rel(owned, filepath.Dir(path))
	if err != nil {
		return err
	}
	current := owned
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			// A concurrent write may create the same parent first.
			if err := os.Mkdir(current, 0o700); err == nil {
				continue
			} else if !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("fixture path parent %s is %v, not a directory", current, info.Mode().Type())
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Chmod(mode), file.Close())
}

// Write creates path, relative to Root or below the fixture's own directories,
// with its missing parents and without following links that exist when it is
// called (see writeContained); the default mode is 0600.
func (fixture *cliFixture) Write(path, content string, mode ...os.FileMode) string {
	fixture.t.Helper()
	owned, path, err := fixture.ownedPath(path)
	if err != nil {
		fixture.t.Fatal(err)
	}
	fileMode := os.FileMode(0o600)
	if len(mode) != 0 {
		fileMode = mode[0]
	}
	if err := writeContained(owned, path, []byte(content), fileMode); err != nil {
		fixture.t.Fatal(err)
	}
	return path
}

// Read returns the content of path, relative to Root or absolute, and fails
// the test when it is unreadable.
func (fixture *cliFixture) Read(path string) string {
	fixture.t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(fixture.Root, path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		fixture.t.Fatal(err)
	}
	return string(content)
}

// Env returns the effective value of name in Environment.
func (fixture *cliFixture) Env(name string) string {
	value := ""
	for _, assignment := range fixture.Environment {
		if key, current, ok := strings.Cut(assignment, "="); ok && key == name {
			value = current
		}
	}
	return value
}

// Exec returns the fake installed by withCommands.
func (fixture *cliFixture) Exec() *testkit.ExecFake {
	fixture.t.Helper()
	if fixture.exec == nil {
		fixture.t.Fatal("fixture has no withCommands fake")
	}
	return fixture.exec
}

// New builds the CLI; RepositoryRoot, Environment and WorkingDir default to
// the fixture's Root, Environment and WorkingDir.
func (fixture *cliFixture) New(options Options) *CLI {
	fixture.t.Helper()
	if options.RepositoryRoot == "" {
		options.RepositoryRoot = fixture.Root
	}
	if options.Environment == nil {
		options.Environment = fixture.Environment
	}
	if options.WorkingDir == "" {
		options.WorkingDir = fixture.WorkingDir
	}
	return mustNew(fixture.t, options)
}

// mustNew builds the CLI without supplying fixture defaults.
func mustNew(t *testing.T, options Options) *CLI {
	t.Helper()
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return program
}

type fixtureRun struct {
	Code   int
	Stdout string
	Stderr string
}

// Run executes arguments, when given, with options as New fills them and
// captures the output streams that options leave unset.
func (fixture *cliFixture) Run(options Options, arguments ...string) fixtureRun {
	fixture.t.Helper()
	if len(arguments) != 0 {
		options.Arguments = arguments
	}
	var stdout, stderr bytes.Buffer
	if options.Stdout == nil {
		options.Stdout = &stdout
	}
	if options.Stderr == nil {
		options.Stderr = &stderr
	}
	code := fixture.New(options).Run(context.Background())
	return fixtureRun{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

func TestCLIFixtureNativeFlavour(t *testing.T) {
	fixture := newFixture(t,
		withEnv("NESTED_E2E_VMS=1", "SSH_PORT=2223"), withoutEnv("DEV_SUDO"), withAudit(),
		withYards("named"), withCommands("ssh", "scripts/e2e-lab/invoke.sh"),
	)
	if info, err := os.Stat(fixture.Root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("root mode: %v %v", info, err)
	}
	if fixture.Home != filepath.Join(fixture.Root, "home") || fixture.ConfigHome != filepath.Join(fixture.Root, "state") ||
		fixture.DataHome != filepath.Join(fixture.Root, "data") {
		t.Fatalf("homes: %#v", fixture)
	}
	if fixture.Env("SSH_PORT") != "2223" || fixture.Env("NESTED_E2E_VMS") != "1" ||
		slices.ContainsFunc(fixture.Environment, func(assignment string) bool {
			return strings.HasPrefix(assignment, "DEV_SUDO=") || strings.HasPrefix(assignment, "SUBYARD_NO_AUDIT=")
		}) {
		t.Fatalf("environment: %q", fixture.Environment)
	}
	if info, err := os.Stat(filepath.Join(fixture.ConfigHome, "yards", "named")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("yard registration directory: %v %v", info, err)
	}
	path := fixture.Write("state/yards/named/config.env", "SSH_PORT=2224\n")
	executable := fixture.Write(filepath.Join(fixture.Root, "new", "tool"), "#!/bin/sh\n", 0o750)
	for file, mode := range map[string]os.FileMode{path: 0o600, executable: 0o750} {
		if info, err := os.Stat(file); err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s mode: %v %v", file, info, err)
		}
	}
	if fixture.Read(path) != "SSH_PORT=2224\n" || fixture.Read("state/yards/named/config.env") != "SSH_PORT=2224\n" {
		t.Fatal("fixture read differs")
	}
	if !strings.HasPrefix(os.Getenv("PATH"), fixture.Bin+string(os.PathListSeparator)) || fixture.Env("PATH") != os.Getenv("PATH") {
		t.Fatalf("PATH=%q environment PATH=%q", os.Getenv("PATH"), fixture.Env("PATH"))
	}
	if fixture.Exec().Path("scripts/e2e-lab/invoke.sh") != filepath.Join(fixture.Root, "scripts", "e2e-lab", "invoke.sh") {
		t.Fatal("repository tool is not below Root")
	}
	fixture.Exec().On(testkit.Match{Tool: "ssh", Args: []string{"-V"}}, testkit.Reply{Stdout: "fake ssh\n"})
	if output, err := exec.Command("ssh", "-V").Output(); err != nil || string(output) != "fake ssh\n" {
		t.Fatalf("PATH shim: %q %v", output, err)
	}
	fixture.Exec().AssertCalls(testkit.Call{Tool: "ssh", Args: []string{"-V"}})
	program := fixture.New(Options{})
	if program.options.RepositoryRoot != fixture.Root || program.options.WorkingDir != fixture.Root ||
		!slices.Equal(program.options.Environment, fixture.Environment) {
		t.Fatalf("defaults: %#v", program.options)
	}
	if run := fixture.Run(Options{}, "yards", "--help"); run.Code != 0 || !strings.Contains(run.Stdout, "yards") {
		t.Fatalf("run: %#v", run)
	}
}

func TestCLIFixtureCheckoutConfigIsAFixtureOwnedCopy(t *testing.T) {
	fixture := newFixture(t, withCheckoutConfig(), withCommands("scripts/e2e-lab/invoke.sh"))
	checkout := repositoryRoot(t)
	if fixture.Root == checkout || fixture.WorkingDir != fixture.Root ||
		fixture.ConfigHome != filepath.Join(fixture.Home, ".config", "subyard") || fixture.DataHome != filepath.Join(fixture.Home, ".subyard") {
		t.Fatalf("checkout fixture: %#v", fixture)
	}
	if info, err := os.Stat(filepath.Dir(fixture.Home)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private parent mode: %v %v", info, err)
	}
	// Root and the private homes hold only directories and regular files, so
	// no write below them can reach the checkout.
	top := map[string]bool{}
	for _, directory := range []string{fixture.Root, filepath.Dir(fixture.Home)} {
		err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				t.Errorf("%s is %v", path, info.Mode())
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && info.Mode().IsRegular() && stat.Nlink != 1 {
				t.Errorf("%s has %d links", path, stat.Nlink)
			}
			if relative, _ := filepath.Rel(fixture.Root, path); directory == fixture.Root && relative != "." && !strings.Contains(relative, "/") {
				top[relative] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !maps.Equal(top, map[string]bool{"bin": true, "completions": true, "config": true, "scripts": true}) {
		t.Fatalf("Root holds %v", slices.Sorted(maps.Keys(top)))
	}
	if fixture.Exec().Path("scripts/e2e-lab/invoke.sh") != filepath.Join(fixture.Root, "scripts", "e2e-lab", "invoke.sh") {
		t.Fatal("repository tool is not installed in the copy")
	}
	if fixture.Read("bin/yard") != fixture.Read(filepath.Join(checkout, "bin", "yard")) {
		t.Fatal("copied bin/yard differs from the checkout")
	}
	if fixture.Env("SUBYARD_CONFIG_DIR") != filepath.Join(fixture.Root, "config") {
		t.Fatalf("SUBYARD_CONFIG_DIR=%q", fixture.Env("SUBYARD_CONFIG_DIR"))
	}
	// A shipped default changed in the copy is what the configuration load reads.
	shipped := fixture.Read("config/subyard.env")
	if !strings.Contains(shipped, `: "${YARD_INSTANCE_NAME:=yard}"`) {
		t.Fatal("shipped YARD_INSTANCE_NAME default moved")
	}
	fixture.Write("config/subyard.env", strings.Replace(shipped, `: "${YARD_INSTANCE_NAME:=yard}"`, `: "${YARD_INSTANCE_NAME:=copied-yard}"`, 1))
	loaded, err := fixture.New(Options{}).loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Context.YardInstanceName != "copied-yard" || loaded.Context.Paths.ConfigDir != filepath.Join(fixture.Root, "config") {
		t.Fatalf("loaded instance=%q config=%q", loaded.Context.YardInstanceName, loaded.Context.Paths.ConfigDir)
	}
}

func TestCLIFixtureRefusesPathsAndSettingsItDoesNotOwn(t *testing.T) {
	fixture := newFixture(t, withCheckoutConfig())
	private := filepath.Dir(fixture.Home)
	for path, want := range map[string]string{
		"config/new":                 filepath.Join(fixture.Root, "config", "new"),
		"private/yards/neighbor.env": filepath.Join(fixture.Root, "private", "yards", "neighbor.env"),
		filepath.Join(fixture.ConfigHome, "config.env"): filepath.Join(fixture.ConfigHome, "config.env"),
		filepath.Join(fixture.Bin, "tool"):              filepath.Join(fixture.Bin, "tool"),
		"../outside":                                    "",
		"/etc/subyard":                                  "",
		filepath.Join(fixture.Root, "..", "outside"):    "",
		fixture.Root + "-sibling":                       "",
		private + "-sibling":                            "",
		filepath.Join(repositoryRoot(t), "config"):      "",
	} {
		if _, got, err := fixture.ownedPath(path); got != want || (err != nil) != (want == "") {
			t.Errorf("%s: %q %v", path, got, err)
		}
	}
	// Containment holds when the file is opened: a link a test created later
	// below Root is not followed, as a parent or as the file itself.
	outside := testkit.TempDir(t)
	if err := errors.Join(os.Symlink(outside, filepath.Join(fixture.Root, "linked")),
		os.Symlink(filepath.Join(outside, "target"), filepath.Join(fixture.Root, "config", "linked"))); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"linked/file", "linked/new/file", "config/linked"} {
		if err := writeContained(fixture.Root, filepath.Join(fixture.Root, path), []byte("x"), 0o600); err == nil {
			t.Errorf("%s: write followed a link", path)
		}
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("write reached outside the fixture: %v %v", entries, err)
	}
	// Concurrent writes of distinct files share their missing parents.
	var writers sync.WaitGroup
	failures := make(chan error, 24)
	for writer := range 24 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			path := filepath.Join(fixture.Root, "shared", "missing", "parent", fmt.Sprintf("file-%d", writer))
			failures <- writeContained(fixture.Root, path, []byte("x"), 0o600)
		}()
	}
	writers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}
	for tool, local := range map[string]bool{
		"scripts/e2e-lab/invoke.sh": true, "../outside/tool": false, "/usr/bin/tool": false,
		"scripts/../tool": false, "scripts//tool": false, "./tool": false,
	} {
		if got := localCommandPath(tool); got != local {
			t.Errorf("%s: local=%t", tool, got)
		}
	}
	for name, fixed := range map[string]bool{
		"HOME": true, "SUBYARD_OPERATOR_HOME": true, "SUBYARD_CONFIG_HOME": true, "XDG_CONFIG_HOME": true,
		"SUBYARD_HOME": true, "SUBYARD_CONFIG_DIR": true, "PATH": true, "SUBYARD_NO_AUDIT": false, "SSH_PORT": false,
	} {
		if fixedSetting(name) != fixed {
			t.Errorf("%s: fixed=%t", name, !fixed)
		}
	}
}

func TestCLIFixtureCheckoutListingFailsClosed(t *testing.T) {
	export := testkit.TempDir(t)
	// A repository above the test directory must not adopt the export.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(export))
	for _, tree := range checkoutTrees {
		if err := os.Mkdir(filepath.Join(export, tree), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(export, "config", "subyard.env"), nil, 0o600)
	if files, err := trackedCheckoutFiles(export); err != nil || !slices.Equal(files, []string{filepath.Join("config", "subyard.env")}) {
		t.Fatalf("export listing: %q %v", files, err)
	}
	// A source below a Git top level lists only its tracked files.
	repository, err := os.MkdirTemp("/tmp", "subyard-fixture-listing-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repository) })
	nested := filepath.Join(repository, "nested")
	for _, tree := range checkoutTrees {
		if err := os.MkdirAll(filepath.Join(nested, tree), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, filepath.Join(nested, "config", "tracked.env"), nil, 0o600)
	runConfigSyncGit(t, repository, "init", "-q")
	runConfigSyncGit(t, repository, "add", "nested/config/tracked.env")
	testkit.WriteFile(t, filepath.Join(nested, "config", "untracked.env"), nil, 0o600)
	if files, err := trackedCheckoutFiles(nested); err != nil || !slices.Equal(files, []string{filepath.Join("config", "tracked.env")}) {
		t.Fatalf("nested listing: %q %v", files, err)
	}
	// A checkout whose Git lookup fails must not fall back to the walk, which
	// would copy untracked files.
	testkit.WriteFile(t, filepath.Join(export, ".git"), []byte("gitdir: "+filepath.Join(export, "missing")+"\n"), 0o600)
	if files, err := trackedCheckoutFiles(export); err == nil {
		t.Fatalf("broken checkout listed %q", files)
	}
}
