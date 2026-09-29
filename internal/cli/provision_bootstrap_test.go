package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type profileInstallFixture struct {
	t         *testing.T
	platform  *initPlatformFixture
	installed bool
	fail      bool
	applies   int
}

func TestProvisionBootstrapPreservesGroupReexecContinuation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "sample")
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment,
		InitPlatform: newInitPlatformFixture()})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := program.prepareProfileBootstrap(context.Background(), loaded, "sample", "provision", nil)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.init == nil {
		t.Fatal("fresh profile skipped init")
	}
	program.options.InitPlatform = nil
	bootstrap.init.rebuildPlatform(program)
	runtime := bootstrap.init.platform.(reconcileruntime.Runtime)
	if runtime.ProvisionProfile != "sample" {
		t.Fatalf("group reexec lost provision continuation: %q", runtime.ProvisionProfile)
	}
}

func (f *profileInstallFixture) Run(_ context.Context, request domain.AdapterRequest, _ io.Reader) (domain.AdapterResult, string, error) {
	result := domain.AdapterResult{Schema: 1, OperationID: request.OperationID, Status: "ok"}
	switch request.Action {
	case "profile-check":
		if f.installed {
			return result, "converged", nil
		}
		return result, "changed", nil
	case "profile":
		if !f.platform.converged[ports.ReconcileStageExtras] {
			f.t.Fatal("profile ran before prerequisites")
		}
		f.applies++
		if f.fail {
			return result, "", errors.New("fixture installation failed")
		}
		f.installed = true
		return result, "", nil
	default:
		f.t.Fatalf("unexpected adapter request: %s/%s", request.Adapter, request.Action)
		return result, "", nil
	}
}

func TestProvisionActivatesProfile(t *testing.T) {
	for _, yard := range []string{"default", "demo"} {
		for _, scenario := range []string{"apply", "decline", "stale", "source appeared", "init failure", "hook failure", "no hook"} {
			t.Run(yard+"/"+scenario, func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				writeProvisionProfile(t, root, "sample")
				if scenario == "no hook" {
					if err := os.Remove(filepath.Join(root, "config/profiles/sample/provision.sh")); err != nil {
						t.Fatal(err)
					}
				}
				platform := convergedProvisionInit(t, root)
				platform.converged[ports.ReconcileStageExtras] = false
				runner := &profileInstallFixture{t: t, platform: platform, fail: scenario == "hook failure"}
				if scenario == "init failure" {
					platform.applyErr = errors.New("fixture init failed")
				}
				path := filepath.Join(root, "state", "yards", yard, "config.env")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				original := "ENVIRONMENT_PROFILES=existing\nSSH_PORT=2345\n"
				writeCLIFile(t, path, original, 0o600)
				incus := lifecycleIncus()
				var stderr bytes.Buffer
				prompt := &callbackPrompt{callback: func() {
					content, _ := os.ReadFile(path)
					if string(content) != original || len(platform.applied) != 0 || runner.applies != 0 {
						t.Fatal("mutated before consent")
					}
					if scenario == "source appeared" {
						if err := configsync.RegisterSource(filepath.Join(root, "state"), filepath.Join(root, "checkout")); err != nil {
							t.Fatal(err)
						}
					}
					if scenario == "stale" {
						writeCLIFile(t, path, original+"# concurrent edit\n", 0o600)
					}
				}}
				options := Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"-Y", yard, "provision", "sample"}, Environment: environment, WorkingDir: root, Incus: incus, InitPlatform: platform, AdapterRunner: runner, Prompt: prompt, Stderr: &stderr}
				if scenario == "decline" {
					options.Prompt = &testkit.Prompt{Answers: []bool{false}}
				}
				program, err := New(options)
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := program.resolveContextWithYardSettings(yard, "")
				if err != nil {
					t.Fatal(err)
				}
				instance := incus.Instances["subyard/yard"]
				instance.Name, instance.Project, instance.Status = loaded.Context.YardInstanceName, loaded.Context.IncusProject, "Running"
				instance.Config["user.subyard.name"] = yard
				instance.Config["user.subyard.desired_power"] = "running"
				incus.Instances[instance.Project+"/"+instance.Name] = instance
				code := program.Run(context.Background())
				want := 0
				if scenario != "apply" && scenario != "no hook" {
					want = 1
				}
				if code != want {
					t.Fatalf("code=%d want=%d stderr=%s", code, want, stderr.String())
				}
				if scenario != "decline" && (len(prompt.requests) != 1 || prompt.requests[0].Default != "yes") {
					t.Fatalf("prompts=%v", prompt.requests)
				}
				content, _ := os.ReadFile(path)
				if scenario == "decline" || scenario == "stale" || scenario == "source appeared" {
					if strings.Contains(string(content), "sample") || len(platform.applied) != 0 || runner.applies != 0 {
						t.Fatal("declined/stale operation mutated state")
					}
					return
				}
				if !strings.Contains(string(content), "existing sample") || !strings.Contains(string(content), "SSH_PORT=2345") {
					t.Fatalf("selection/settings lost: %s", content)
				}
				if _, err := os.Stat(filepath.Join(root, "state", "config.env")); !os.IsNotExist(err) {
					t.Fatal("modified host settings")
				}
				// Retry after either partial failure, then a converged repeat must be a no-op.
				platform.applyErr, runner.fail = nil, false
				retryPrompt := &testkit.Prompt{Answers: []bool{true}}
				options.Prompt = retryPrompt
				for i := 0; i < 2; i++ {
					retry, err := New(options)
					if err != nil {
						t.Fatal(err)
					}
					if code := retry.Run(context.Background()); code != 0 {
						t.Fatalf("retry=%d stderr=%s", code, stderr.String())
					}
				}
				expectedPrompts := 0
				if scenario == "init failure" || scenario == "hook failure" {
					expectedPrompts = 1
				}
				if len(retryPrompt.Requests) != expectedPrompts {
					t.Fatalf("repeat prompts=%d", len(retryPrompt.Requests))
				}
				expectedApplies := 1
				if scenario == "hook failure" {
					expectedApplies = 2
				}
				if scenario == "no hook" {
					expectedApplies = 0
				}
				if runner.applies != expectedApplies {
					t.Fatalf("applies=%d want=%d", runner.applies, expectedApplies)
				}
				content, _ = os.ReadFile(path)
				if strings.Count(string(content), "sample") != 1 {
					t.Fatalf("duplicate selection: %s", content)
				}
			})
		}
	}
}

func TestProvisionActivationRejectsSourceManagedSelection(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "sample")
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := configsync.RegisterSource(filepath.Join(root, "state"), filepath.Join(root, "checkout")); err != nil {
		t.Fatal(err)
	}
	_, err = program.prepareProfileBootstrap(context.Background(), loaded, "sample", "provision", nil)
	if err == nil || !strings.Contains(err.Error(), "source-managed") {
		t.Fatalf("guard=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "yards", "default", "config.env")); !os.IsNotExist(err) {
		t.Fatal("source-managed selection written")
	}
}
