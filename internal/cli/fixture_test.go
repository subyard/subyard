package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// cliFixture is one CLI test environment. The default flavour is a synthetic
// repository root with private homes below Root. withCheckoutConfig uses an
// isolated copy of the checkout instead: config/ is copied, private/ is absent
// and the other top-level entries link to the checkout, with private homes in
// a temporary directory. Home, ConfigHome and DataHome are the effective homes
// of Environment as newFixture leaves it.
type cliFixture struct {
	t           *testing.T
	Root        string
	Home        string
	ConfigHome  string
	DataHome    string
	Bin         string
	WorkingDir  string
	Environment []string
	checkout    string
	exec        *testkit.ExecFake
}

// fixtureOption configures newFixture. Options apply in the order given,
// after the flavour's base setup.
type fixtureOption struct {
	checkout bool
	apply    func(*cliFixture)
}

// withCheckoutConfig uses an isolated copy of the checkout as RepositoryRoot,
// as checkoutMirrorFixture builds it, with the shipped config read from the
// copy. A test may seed private/ below Root; the checkout itself is never written.
func withCheckoutConfig() fixtureOption { return fixtureOption{checkout: true} }

// withWorkingDir sets the WorkingDir that New uses when Options leave it
// empty; an empty path keeps it empty.
func withWorkingDir(path string) fixtureOption {
	return fixtureOption{apply: func(fixture *cliFixture) { fixture.WorkingDir = path }}
}

// withCommands installs an ExecFake shim for each tool. A bare name goes to Bin,
// which is put first on PATH for the test process and in Environment; a name
// with a slash is a local path below Root, such as a repository script.
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
			fixture.exec.Install(tool, fixture.writable(tool))
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

// withEnv appends NAME=value assignments; the last assignment of a name wins.
func withEnv(assignments ...string) fixtureOption {
	return fixtureOption{apply: func(fixture *cliFixture) {
		for _, assignment := range assignments {
			if !strings.Contains(assignment, "=") {
				fixture.t.Fatalf("fixture environment %q is not NAME=value", assignment)
			}
		}
		fixture.Environment = slices.Clip(append(fixture.Environment, assignments...))
	}}
}

// withoutEnv removes every assignment of the names.
func withoutEnv(names ...string) fixtureOption {
	return fixtureOption{apply: func(fixture *cliFixture) {
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
		for _, name := range names {
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
		root, home, _, environment := configCommandFixture(t)
		if err := os.Chmod(filepath.Dir(home), 0o700); err != nil {
			t.Fatal(err)
		}
		fixture.checkout, fixture.Root = root, checkoutMirrorFixture(t, root)
		fixture.Environment = append(slices.DeleteFunc(environment, func(assignment string) bool {
			return strings.HasPrefix(assignment, "SUBYARD_CONFIG_DIR=")
		}), "SUBYARD_CONFIG_DIR="+filepath.Join(fixture.Root, "config"))
	} else {
		root, environment, _ := nativeFixture(t)
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
		fixture.Root, fixture.Environment = root, environment
	}
	fixture.Environment = slices.Clip(fixture.Environment)
	fixture.WorkingDir = fixture.Root
	fixture.Bin = testkit.TempDir(t)
	fixture.resolveHomes()
	for _, option := range options {
		if option.apply != nil {
			option.apply(fixture)
			fixture.resolveHomes()
		}
	}
	return fixture
}

// resolveHomes derives the homes from Environment as the product does.
func (fixture *cliFixture) resolveHomes() {
	fixture.Home = fixture.Env("HOME")
	operator := firstNonempty(fixture.Env("SUBYARD_OPERATOR_HOME"), fixture.Home)
	fixture.ConfigHome = firstNonempty(fixture.Env("SUBYARD_CONFIG_HOME"),
		filepath.Join(firstNonempty(fixture.Env("XDG_CONFIG_HOME"), filepath.Join(operator, ".config")), "subyard"))
	fixture.DataHome = firstNonempty(fixture.Env("SUBYARD_HOME"), filepath.Join(operator, ".subyard"))
}

// path resolves a fixture path; a relative path is below Root.
func (fixture *cliFixture) path(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(fixture.Root, path)
}

// writable resolves path and fails the test when it reaches into the checkout.
func (fixture *cliFixture) writable(path string) string {
	fixture.t.Helper()
	path = fixture.path(path)
	if fixture.checkout == "" {
		return path
	}
	inside, err := reachesInto(path, fixture.checkout)
	if err != nil {
		fixture.t.Fatal(err)
	}
	if inside {
		fixture.t.Fatalf("fixture write %s reaches into the checkout", path)
	}
	return path
}

// reachesInto reports whether path, with its symlinks resolved, is root or below it.
func reachesInto(path, root string) (bool, error) {
	resolved, err := resolveExisting(path)
	if err != nil {
		return false, err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return false, err
	}
	relative, err := filepath.Rel(root, resolved)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
}

// resolveExisting resolves the symlinks of the longest existing ancestor of
// path. A dangling symlink on the way is an error: a write would follow it to
// a target that the check cannot place.
func resolveExisting(path string) (string, error) {
	missing := ""
	for current := path; ; current = filepath.Dir(current) {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(resolved, missing), nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(current) == current {
			return "", err
		}
		if _, err := os.Lstat(current); err == nil {
			return "", fmt.Errorf("fixture path %s crosses the dangling symlink %s", path, current)
		}
		missing = filepath.Join(filepath.Base(current), missing)
	}
}

// Write creates path with its missing parents; the default mode is 0600.
func (fixture *cliFixture) Write(path, content string, mode ...os.FileMode) string {
	fixture.t.Helper()
	path = fixture.writable(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fixture.t.Fatal(err)
	}
	fileMode := os.FileMode(0o600)
	if len(mode) != 0 {
		fileMode = mode[0]
	}
	testkit.WriteFile(fixture.t, path, []byte(content), fileMode)
	return path
}

// Read returns the content of path and fails the test when it is unreadable.
func (fixture *cliFixture) Read(path string) string {
	fixture.t.Helper()
	content, err := os.ReadFile(fixture.path(path))
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
	program, err := New(options)
	if err != nil {
		fixture.t.Fatal(err)
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

func TestCLIFixtureCheckoutConfigIsAnIsolatedCopy(t *testing.T) {
	fixture := newFixture(t, withCheckoutConfig())
	checkout := repositoryRoot(t)
	if fixture.Root == checkout || fixture.checkout != checkout || fixture.WorkingDir != fixture.Root ||
		fixture.ConfigHome != filepath.Join(fixture.Home, ".config", "subyard") || fixture.DataHome != filepath.Join(fixture.Home, ".subyard") {
		t.Fatalf("checkout fixture: %#v", fixture)
	}
	if info, err := os.Stat(filepath.Dir(fixture.Home)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private parent mode: %v %v", info, err)
	}
	if info, err := os.Lstat(filepath.Join(fixture.Root, "config")); err != nil || !info.IsDir() {
		t.Fatalf("config is not a copied directory: %v %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.Root, "private")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private/ is present in the copy: %v", err)
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
	overlay := fixture.Write("private/yards/neighbor.env", "SSH_PORT=2399\n")
	dangling := filepath.Join(fixture.Root, "dangling")
	if err := os.Symlink(filepath.Join(checkout, "config", "absent-fixture-target"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path   string
		inside bool
		err    bool
	}{
		{path: filepath.Join(checkout, "config", "new", "file"), inside: true},
		{path: filepath.Join(fixture.Root, "scripts", "new"), inside: true},
		{path: checkout, inside: true},
		{path: overlay},
		{path: filepath.Join(fixture.Root, "config", "new")},
		{path: filepath.Join(fixture.Home, "file")},
		{path: checkout + "-sibling"},
		{path: dangling, err: true},
		{path: filepath.Join(dangling, "below"), err: true},
	} {
		if inside, err := reachesInto(test.path, checkout); (err != nil) != test.err || inside != test.inside {
			t.Errorf("%s: inside=%t err=%v", test.path, inside, err)
		}
	}
}

func TestCLIFixtureResolvesHomesWhileApplyingOptions(t *testing.T) {
	override := filepath.Join(testkit.TempDir(t), "config-home")
	fixture := newFixture(t, withEnv("SUBYARD_CONFIG_HOME="+override), withYards("named"),
		withoutEnv("SUBYARD_HOME"), withWorkingDir(""))
	if fixture.ConfigHome != override || fixture.DataHome != filepath.Join(fixture.Home, ".subyard") || fixture.WorkingDir != "" {
		t.Fatalf("homes: config=%q data=%q working=%q", fixture.ConfigHome, fixture.DataHome, fixture.WorkingDir)
	}
	if info, err := os.Stat(filepath.Join(override, "yards", "named")); err != nil || !info.IsDir() {
		t.Fatalf("yard registered outside the effective config home: %v %v", info, err)
	}
	if program := fixture.New(Options{}); program.options.WorkingDir != "" {
		t.Fatalf("WorkingDir=%q", program.options.WorkingDir)
	}
}

func TestCLIFixtureCommandPathsStayBelowRoot(t *testing.T) {
	for tool, local := range map[string]bool{
		"scripts/e2e-lab/invoke.sh": true, "../outside/tool": false, "/usr/bin/tool": false,
		"scripts/../tool": false, "scripts//tool": false, "./tool": false,
	} {
		if got := localCommandPath(tool); got != local {
			t.Errorf("%s: local=%t", tool, got)
		}
	}
}
