package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func emptyNamedFixture(t *testing.T) *cliFixture {
	t.Helper()
	fixture := newFixture(t, withWorkingDir(""), withoutEnv("SSH_PORT", "HOST_BASE", "RESTRICTED_DISK_PATHS"))
	path := filepath.Join("config", "subyard.env")
	fixture.Write(path, strings.ReplaceAll(fixture.Read(path), filepath.Join(fixture.Root, "host"), "/srv/subyard"))
	return fixture
}

func TestInitEmptyNamedYardCreationAndRetry(t *testing.T) {
	for _, scenario := range []string{"create", "decline", "preflight", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := emptyNamedFixture(t)
			root := fixture.Root
			// Inherited selections must not select a preset, profile or tool.
			writeEmptyNamedFile(t, filepath.Join(root, "state", "config.env"), "ENVIRONMENT_PROFILES=sample\nCODING_TOOL_INTEGRATIONS=codex\n", 0600)
			writeEmptyNamedFile(t, filepath.Join(root, "config", "profiles", "tools.env"), "CODING_TOOL_INTEGRATIONS=codex\n", 0600)
			platform := newInitPlatformFixture()
			if scenario == "preflight" {
				platform.preflightErr = errors.New("unsupported host")
			}
			if scenario == "retry" {
				platform.applyErr = errors.New("interrupted")
			}
			var stdout, stderr bytes.Buffer
			program := fixture.New(Options{
				Arguments: []string{"-Y", "fresh", "init"}, InitPlatform: platform,
				Prompt: &testkit.Prompt{Answers: []bool{scenario != "decline"}}, Stdout: &stdout, Stderr: &stderr})
			code := program.Run(context.Background())
			if (code == 0) != (scenario == "create") {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			path := filepath.Join(root, "state", "yards", "fresh", "config.env")
			content, err := os.ReadFile(path)
			if scenario == "decline" || scenario == "preflight" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unapproved registration was published", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err, stderr.String())
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("registration is not protected", err)
			}
			if !strings.Contains(stdout.String(), "init.registration") || !strings.Contains(stdout.String(), "owner-local SSH port") {
				t.Fatal("registration and endpoint omitted from typed plan")
			}
			retry := fixture.New(Options{})
			loaded, bootstrap, err := retry.loadInitContext("fresh", true, nil)
			if err != nil || bootstrap != nil || loaded.Environment["YARD_TEMPLATE"] != "" || len(loaded.EnvironmentProfiles.Effective) != 0 || len(loaded.Integrations.Effective) != 0 || loaded.Context.SSHPort == 2222 {
				t.Fatalf("retry lost explicit empty selection/endpoint: bootstrap=%v context=%+v err=%v", bootstrap, loaded.Context, err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(content, after) {
				t.Fatal("retry rewrote registration")
			}
		})
	}
}

func TestInitEmptyNamedYardPreservesRegisteredSelectionAndUnknownModes(t *testing.T) {
	fixture := emptyNamedFixture(t)
	root := fixture.Root
	path := filepath.Join(root, "state", "yards", "saved", "config.env")
	writeEmptyNamedFile(t, path, "SSH_PORT=2444\nENVIRONMENT_PROFILES=sample\nCODING_TOOL_INTEGRATIONS=codex\n", 0600)
	program := fixture.New(Options{})
	loaded, bootstrap, err := program.loadInitContext("saved", true, nil)
	if err != nil || bootstrap != nil || loaded.Environment["ENVIRONMENT_PROFILES"] != "sample" || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
		t.Fatalf("registered selection changed: %v", err)
	}
	for _, arguments := range [][]string{{"--configs"}, {"--reset"}, {"--continue-legacy"}, {"--adopt"}} {
		if _, _, err := program.loadInitContext("missing", true, arguments); !errors.Is(err, config.ErrUnknownYard) {
			t.Fatalf("mode %v implicitly created missing name: %v", arguments, err)
		}
	}
	if _, err := program.loadContext("missing"); !errors.Is(err, config.ErrUnknownYard) {
		t.Fatal("ordinary loader admitted missing name", err)
	}
	if _, _, err := program.loadInitContext("../invalid", true, nil); err == nil {
		t.Fatal("invalid name accepted")
	}
}

func TestInitEmptyNamedYardRegistrationRaces(t *testing.T) {
	for _, race := range []string{"name", "port", "listener", "inputs", "namespace"} {
		t.Run(race, func(t *testing.T) {
			fixture := emptyNamedFixture(t)
			root := fixture.Root
			program := fixture.New(Options{InitPlatform: newInitPlatformFixture()})
			loaded, bootstrap, err := program.loadInitContext("fresh", true, nil)
			if err != nil {
				t.Fatal(err)
			}
			execution := &initExecution{loaded: loaded, bootstrap: bootstrap}
			execution.inputBaseline, err = program.captureOwnerInputs(loaded, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			switch race {
			case "name":
				writeEmptyNamedFile(t, bootstrap.targetPath, "SSH_PORT=2555\n", 0600)
			case "port":
				writeEmptyNamedFile(t, filepath.Join(root, "state", "yards", "other", "config.env"), "SSH_PORT="+loaded.Environment["SSH_PORT"]+"\n", 0600)
			case "listener":
				listener, err := net.Listen("tcp4", "127.0.0.1:"+loaded.Environment["SSH_PORT"])
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
			case "inputs":
				writeEmptyNamedFile(t, filepath.Join(root, "state", "config.env"), "LIMITS_CPU=2\n", 0600)
			case "namespace":
				program.options.InitPlatform = nil
				program.options.Incus = &testkit.Incus{Reconcile: ports.ReconcileState{ProjectFound: true}}
			}
			if err := execution.publishRegistration(context.Background(), program); err == nil {
				t.Fatal("changed registration inputs accepted")
			}
			if race != "name" {
				if _, err := os.Stat(bootstrap.targetPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("stale plan published registration", err)
				}
			}
		})
	}
}

func writeEmptyNamedFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(content), mode)
}

func TestInitEmptyNamedYardConcurrentPortPublication(t *testing.T) {
	fixture := emptyNamedFixture(t)
	executions := make([]*initExecution, 2)
	programs := make([]*CLI, 2)
	for i, name := range []string{"first", "second"} {
		program := fixture.New(Options{InitPlatform: newInitPlatformFixture()})
		loaded, bootstrap, err := program.loadInitContext(name, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		programs[i], executions[i] = program, &initExecution{loaded: loaded, bootstrap: bootstrap}
	}
	if executions[0].loaded.Context.SSHPort != executions[1].loaded.Context.SSHPort {
		t.Fatal("fixture did not plan a competing port")
	}
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for i := range executions {
		workers.Go(func() { results <- executions[i].publishRegistration(context.Background(), programs[i]) })
	}
	workers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("competing registration publications succeeded %d times", successes)
	}
}

type registrationRootProbe struct {
	*testkit.Incus
	root   string
	called bool
}

func (probe *registrationRootProbe) ReconcileState(context.Context, string, string, string, string, string) (ports.ReconcileState, error) {
	probe.called = true
	reader, err := os.Open(probe.root)
	if err != nil {
		return ports.ReconcileState{}, err
	}
	defer reader.Close()
	err = syscall.Flock(int(reader.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(reader.Fd()), syscall.LOCK_UN)
		return ports.ReconcileState{}, errors.New("registration recheck does not hold the exclusive config-root lock")
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return ports.ReconcileState{}, err
	}
	return ports.ReconcileState{}, nil
}

func TestInitRegistrationChecksRunUnderRootWriteLock(t *testing.T) {
	fixture := emptyNamedFixture(t)
	root := fixture.Root
	probe := &registrationRootProbe{Incus: &testkit.Incus{}, root: filepath.Join(root, "state")}
	program := fixture.New(Options{Incus: probe})
	loaded, bootstrap, err := program.loadInitContext("fresh", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	execution := &initExecution{loaded: loaded, bootstrap: bootstrap}
	execution.inputBaseline, err = program.captureOwnerInputs(loaded, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := execution.publishRegistration(context.Background(), program); err != nil {
		t.Fatal(err)
	}
	if !probe.called {
		t.Fatal("registration skipped its native namespace recheck")
	}
	if _, err := os.Stat(bootstrap.targetPath); err != nil {
		t.Fatal("checked registration was not published", err)
	}
}

func TestInitNamedYardCreationProfiles(t *testing.T) {
	fixture := emptyNamedFixture(t)
	root := fixture.Root
	writeEmptyNamedFile(t, filepath.Join(root, "config", "profiles", "alpha", "profile.json"), `{"schema_version":1}`, 0600)
	writeEmptyNamedFile(t, filepath.Join(root, "config", "profiles", "beta", "profile.json"), `{"schema_version":1}`, 0600)
	writeEmptyNamedFile(t, filepath.Join(root, "config", "profiles", "off", "profile.json"), `{"schema_version":1,"disabled":true}`, 0600)
	// Each load adopts its context, so every attempt uses a fresh program.
	load := func(yard string, explicit bool, arguments ...string) (config.Loaded, *initBootstrap, error) {
		return fixture.New(Options{}).loadInitContext(yard, explicit, arguments)
	}
	loaded, bootstrap, err := load("fresh", true, "--environment-profiles", "beta  alpha")
	if err != nil || bootstrap == nil {
		t.Fatalf("creation with profiles: %v", err)
	}
	if !strings.Contains(string(bootstrap.content), "\nENVIRONMENT_PROFILES='beta alpha'\nCODING_TOOL_INTEGRATIONS=''\n") ||
		strings.Join(loaded.EnvironmentProfiles.Requested, " ") != "beta alpha" ||
		bootstrap.description() != "create named yard definition with environment profiles [beta alpha] and no coding tools" {
		t.Fatalf("content=%q requested=%v plan=%q", bootstrap.content, loaded.EnvironmentProfiles.Requested, bootstrap.description())
	}
	_, empty, err := load("fresh", true, "--environment-profiles", "")
	if err != nil || empty == nil || !strings.Contains(string(empty.content), "\nENVIRONMENT_PROFILES=''\n") || empty.description() != (&initBootstrap{}).description() {
		t.Fatalf("empty value must be plain creation: %v", err)
	}

	for _, test := range []struct {
		name, yard string
		explicit   bool
		arguments  []string
		want       []string
	}{
		{"default yard", "default", true, []string{"--environment-profiles", "alpha"}, []string{"requires selecting a new non-default yard"}},
		{"implicit yard", "", false, []string{"--environment-profiles", "alpha"}, []string{"requires selecting a new non-default yard"}},
		{"preset", "fresh", true, []string{"--profile", "alpha", "--environment-profiles", "alpha"}, []string{"cannot be combined"}},
		{"configs", "fresh", true, []string{"--configs", "--environment-profiles", "alpha"}, []string{"cannot be combined"}},
		{"reset", "fresh", true, []string{"--environment-profiles", "alpha", "--reset"}, []string{"cannot be combined"}},
		{"adopt", "fresh", true, []string{"--adopt", "--environment-profiles", "alpha"}, []string{"cannot be combined"}},
		{"legacy", "fresh", true, []string{"--continue-legacy", "--environment-profiles", "alpha"}, []string{"cannot be combined"}},
		{"twice", "fresh", true, []string{"--environment-profiles", "alpha", "--environment-profiles", "beta"}, []string{"only once"}},
		{"missing value", "fresh", true, []string{"--environment-profiles"}, []string{"needs a value"}},
		{"names", "fresh", true, []string{"--environment-profiles", "Bad alpha alpha ../x"}, []string{`invalid profile name "Bad"`, `profile "alpha" is repeated`, `invalid profile name "../x"`}},
		{"independent refusals together", "default", true, []string{"--reset", "--environment-profiles", "Bad alpha alpha"},
			[]string{"cannot be combined", "requires selecting a new non-default yard", `invalid profile name "Bad"`, `profile "alpha" is repeated`}},
		{"eligibility", "fresh", true, []string{"--environment-profiles", "missing alpha off ghost"}, []string{`unknown environment profile "missing"`, `profile "off" is not eligible for this yard (policy-disabled)`, `unknown environment profile "ghost"`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, bootstrap, err := load(test.yard, test.explicit, test.arguments...)
			if err == nil || bootstrap != nil {
				t.Fatalf("accepted: %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q lacks %q", err, want)
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "state", "yards", "fresh")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("planning published a registration", err)
	}

	// A rerun after an interrupted creation continues as plain init only for the saved set.
	writeEmptyNamedFile(t, filepath.Join(root, "state", "yards", "resumed", "config.env"), "SSH_PORT=2444\nENVIRONMENT_PROFILES='beta alpha'\n", 0600)
	loaded, bootstrap, err = load("resumed", true, "--environment-profiles", "alpha beta")
	if err != nil || bootstrap != nil || strings.Join(loaded.EnvironmentProfiles.Requested, " ") != "beta alpha" {
		t.Fatalf("equal saved selection must continue as plain init: bootstrap=%v err=%v", bootstrap, err)
	}
	for _, value := range []string{"alpha", "", "alpha beta off"} {
		_, _, err := load("resumed", true, "--environment-profiles", value)
		if err == nil || !strings.Contains(err.Error(), "resume with plain yard -Y resumed init") || !strings.Contains(err.Error(), "profile enable|disable") {
			t.Fatalf("different selection %q: %v", value, err)
		}
	}
	writeEmptyNamedFile(t, filepath.Join(root, "state", "yards", "unsaved", "config.env"), "SSH_PORT=2445\n", 0600)
	if _, _, err := load("unsaved", true, "--environment-profiles", ""); err == nil {
		t.Fatal("a yard without a saved selection accepted a creation selection")
	}
}

func TestInitNamedYardCreationProfilesRefuseCommandOverride(t *testing.T) {
	fixture := emptyNamedFixture(t)
	writeEmptyNamedFile(t, filepath.Join(fixture.Root, "config", "profiles", "alpha", "profile.json"), `{"schema_version":1}`, 0600)
	program := fixture.New(Options{})
	program.env["ENVIRONMENT_PROFILES"] = "beta"
	if _, _, err := program.loadInitContext("fresh", true, []string{"--environment-profiles", "alpha"}); err == nil || !strings.Contains(err.Error(), "command environment overrides new named yard at setting ENVIRONMENT_PROFILES") {
		t.Fatalf("command override accepted: %v", err)
	}
}
