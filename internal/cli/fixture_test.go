package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// cliFixture is one CLI test environment. The default flavour is a synthetic
// repository root with private homes below Root; withCheckoutConfig and
// withCheckoutMirror use the checkout's shipped config with private homes in
// a temporary directory and guard the checkout for the whole test.
type cliFixture struct {
	t           *testing.T
	Root        string
	Home        string
	ConfigHome  string
	DataHome    string
	Bin         string
	Environment []string
	checkout    string
	exec        *testkit.ExecFake
}

// fixtureOption configures newFixture. Options apply in the order given,
// after the flavour's base setup.
type fixtureOption struct {
	checkout bool
	mirror   bool
	apply    func(*cliFixture)
}

// withCheckoutConfig uses the checkout as RepositoryRoot, as configCommandFixture does.
func withCheckoutConfig() fixtureOption { return fixtureOption{checkout: true} }

// withCheckoutMirror uses a temporary mirror of the checkout as RepositoryRoot,
// as checkoutMirrorFixture does, for tests that seed private/.
func withCheckoutMirror() fixtureOption { return fixtureOption{checkout: true, mirror: true} }

// withCommands installs an ExecFake shim for each tool. A bare name goes to Bin,
// which is put first on PATH for the test process and in Environment; a name
// with a slash is a path below Root, such as a repository script.
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
			fixture.exec.Install(tool, fixture.writable(tool))
		}
		if onPath && !slices.Contains(filepath.SplitList(os.Getenv("PATH")), fixture.Bin) {
			fixture.t.Setenv("PATH", fixture.Bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			fixture.Environment = slices.Clip(append(fixture.Environment, "PATH="+os.Getenv("PATH")))
		}
	}}
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
	checkout, mirror := false, false
	for _, option := range options {
		checkout = checkout || option.checkout
		mirror = mirror || option.mirror
	}
	if checkout {
		root, home, _, environment := configCommandFixture(t)
		if err := os.Chmod(filepath.Dir(home), 0o700); err != nil {
			t.Fatal(err)
		}
		guardCheckout(t, root)
		fixture.checkout, fixture.Root, fixture.Environment = root, root, environment
		if mirror {
			fixture.Root = checkoutMirrorFixture(t, root)
		}
	} else {
		root, environment, _ := nativeFixture(t)
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
		fixture.Root, fixture.Environment = root, environment
	}
	fixture.Environment = slices.Clip(fixture.Environment)
	fixture.Home = fixture.Env("HOME")
	fixture.ConfigHome = fixture.Env("SUBYARD_CONFIG_HOME")
	fixture.DataHome = fixture.Env("SUBYARD_HOME")
	fixture.Bin = testkit.TempDir(t)
	for _, option := range options {
		if option.apply != nil {
			option.apply(fixture)
		}
	}
	return fixture
}

// guardCheckout fails the test when it changes the status of the checkout's
// config/ or the optional private/ clone, including ignored files.
func guardCheckout(t *testing.T, root string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if prefix, err := exec.Command(git, "-C", root, "rev-parse", "--show-prefix").Output(); err != nil || strings.TrimSpace(string(prefix)) != "" {
		return
	}
	before := checkoutStatus(t, git, root)
	t.Cleanup(func() {
		if after := checkoutStatus(t, git, root); after != before {
			t.Errorf("test changed checkout config/ or private/:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
}

func checkoutStatus(t *testing.T, git, root string) string {
	t.Helper()
	status := func(directory string, paths ...string) string {
		arguments := append([]string{"-C", directory, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching", "--"}, paths...)
		output, err := exec.Command(git, arguments...).Output()
		if err != nil {
			t.Fatalf("checkout status %s: %v", directory, err)
		}
		return string(output)
	}
	result := status(root, "config", "private")
	private := filepath.Join(root, "private")
	if _, err := os.Stat(filepath.Join(private, ".git")); err == nil {
		// Operator and coordinator documents may change during a run, as in tests/run.sh.
		for _, line := range strings.Split(strings.TrimSuffix(status(private), "\n"), "\n") {
			path := line[min(3, len(line)):]
			if line == "" || strings.HasPrefix(path, "tasks/") || strings.HasPrefix(path, "questions/") ||
				strings.HasPrefix(path, "reference/") || strings.HasPrefix(path, "spec/") ||
				!strings.Contains(path, "/") && strings.HasSuffix(path, ".md") {
				continue
			}
			result += "private/" + line + "\n"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return result
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

// resolveExisting resolves the symlinks of the longest existing ancestor of path.
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
// the fixture's Root, Environment and Root.
func (fixture *cliFixture) New(options Options) *CLI {
	fixture.t.Helper()
	if options.RepositoryRoot == "" {
		options.RepositoryRoot = fixture.Root
	}
	if options.Environment == nil {
		options.Environment = fixture.Environment
	}
	if options.WorkingDir == "" {
		options.WorkingDir = fixture.Root
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

func TestCLIFixtureCheckoutFlavours(t *testing.T) {
	checkout := newFixture(t, withCheckoutConfig())
	if checkout.Root != repositoryRoot(t) || checkout.ConfigHome != filepath.Join(checkout.Home, ".config", "subyard") ||
		checkout.DataHome != filepath.Join(checkout.Home, ".subyard") {
		t.Fatalf("checkout homes: %#v", checkout)
	}
	if info, err := os.Stat(filepath.Dir(checkout.Home)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private parent mode: %v %v", info, err)
	}
	mirror := newFixture(t, withCheckoutMirror())
	if mirror.Root == checkout.Root || mirror.checkout != checkout.Root {
		t.Fatalf("mirror root: %q", mirror.Root)
	}
	overlay := mirror.Write("private/yards/neighbor.env", "SSH_PORT=2399\n")
	for _, test := range []struct {
		path   string
		inside bool
	}{
		{path: filepath.Join(checkout.Root, "config", "new", "file"), inside: true},
		{path: filepath.Join(mirror.Root, "scripts", "new"), inside: true},
		{path: checkout.Root, inside: true},
		{path: overlay},
		{path: filepath.Join(mirror.Root, "config", "new")},
		{path: filepath.Join(checkout.Home, "file")},
		{path: checkout.Root + "-sibling"},
	} {
		if inside, err := reachesInto(test.path, checkout.Root); err != nil || inside != test.inside {
			t.Errorf("%s: inside=%t err=%v", test.path, inside, err)
		}
	}
}

func TestCLIFixtureCheckoutGuardSeesIgnoredAndPrivatePaths(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	for _, directory := range []string{root, filepath.Join(root, "private")} {
		if output, err := exec.Command(git, "init", "-q", directory).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, output)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, ".gitignore"), []byte("/private/\n/config/*.env\n"), 0o600)
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	baseline := checkoutStatus(t, git, root)
	for _, test := range []struct {
		path    string
		changes bool
	}{
		{path: "private/tasks/plan.md"},
		{path: "private/notes.md"},
		{path: "config/local.env", changes: true},
		{path: "config/shipped", changes: true},
		{path: "private/yards/x.env", changes: true},
	} {
		path := filepath.Join(root, test.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte("x\n"), 0o600)
		if changed := checkoutStatus(t, git, root) != baseline; changed != test.changes {
			t.Errorf("%s: changed=%t", test.path, changed)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}
