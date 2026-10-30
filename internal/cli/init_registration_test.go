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

func emptyNamedFixture(t *testing.T) (string, []string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	for _, key := range []string{"SSH_PORT", "HOST_BASE", "RESTRICTED_DISK_PATHS"} {
		environment = withoutCommandSetting(environment, key)
	}
	path := filepath.Join(root, "config", "subyard.env")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(strings.ReplaceAll(string(content), filepath.Join(root, "host"), "/srv/subyard")), 0600)
	return root, environment
}

func TestInitEmptyNamedYardCreationAndRetry(t *testing.T) {
	for _, scenario := range []string{"create", "decline", "preflight", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			root, environment := emptyNamedFixture(t)
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
			program, err := New(Options{RepositoryRoot: root, Environment: environment, Program: "yard",
				Arguments: []string{"-Y", "fresh", "init"}, InitPlatform: platform,
				Prompt: &testkit.Prompt{Answers: []bool{scenario != "decline"}}, Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
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
			retry, err := New(Options{RepositoryRoot: root, Environment: environment, Program: "yard"})
			if err != nil {
				t.Fatal(err)
			}
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
	root, environment := emptyNamedFixture(t)
	path := filepath.Join(root, "state", "yards", "saved", "config.env")
	writeEmptyNamedFile(t, path, "SSH_PORT=2444\nENVIRONMENT_PROFILES=sample\nCODING_TOOL_INTEGRATIONS=codex\n", 0600)
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
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
			root, environment := emptyNamedFixture(t)
			program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: newInitPlatformFixture()})
			if err != nil {
				t.Fatal(err)
			}
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
	root, environment := emptyNamedFixture(t)
	executions := make([]*initExecution, 2)
	programs := make([]*CLI, 2)
	for i, name := range []string{"first", "second"} {
		program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: newInitPlatformFixture()})
		if err != nil {
			t.Fatal(err)
		}
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
	root, environment := emptyNamedFixture(t)
	probe := &registrationRootProbe{Incus: &testkit.Incus{}, root: filepath.Join(root, "state")}
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: probe})
	if err != nil {
		t.Fatal(err)
	}
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
