package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
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

func writeUnmigratedSettingsManifest(t *testing.T, configHome string) {
	t.Helper()
	path := filepath.Join(configHome, ".sync/manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, path, `{"schemaVersion":1,"files":[{"path":"config.env"}]}`, 0o600)
}

func TestSelectedNamedProvisionAssessesOnlyItsNativePrerequisites(t *testing.T) {
	for _, scenario := range []string{"ready", "missing", "unmarked", "foreign"} {
		t.Run(scenario, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			writeProvisionProfile(t, root, "sample")
			writeProvisionProfile(t, root, "other")
			path := filepath.Join(root, "state", "config.env")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, path, "ENVIRONMENT_PROFILES='sample other'\n", 0600)
			runtime, incus := legacyContinuationFixture(t)
			runtime.RepositoryRoot = root
			runtime.Environment = append(runtime.Environment, "ENVIRONMENT_PROFILES=sample other")
			// An unrelated selected profile needs native work. The named guest hook
			// must not acquire its prerequisites or execute its provision hook.
			writeCLIFile(t, filepath.Join(root, "config/profiles/other/profile.conf"), "YARD_DEVICES=kvm\n", 0600)
			runtime.HostDeviceRoot = testkit.TempDir(t)
			if err := os.Symlink("/dev/null", filepath.Join(runtime.HostDeviceRoot, "kvm")); err != nil {
				t.Fatal(err)
			}
			incus.Reconcile.Instance.LocalDevices["kvm"] = map[string]string{"type": "unix-char", "source": "/dev/kvm", "path": "/dev/kvm"}
			if scenario == "missing" {
				writeCLIFile(t, filepath.Join(root, "config/profiles/sample/profile.conf"), "YARD_DEVICES=kvm\n", 0600)
			} else if scenario != "unmarked" {
				incus.Reconcile.ProjectConfig["user.subyard.installation"] = "1"
				incus.Reconcile.ProjectConfig["user.subyard.operator_uid"] = strconv.Itoa(os.Getuid())
				incus.Reconcile.ProjectConfig["user.subyard.data_home"] = runtime.Yard.Paths.DataHome
				incus.Reconcile.ProjectConfig["user.subyard.config_home"] = runtime.Yard.Paths.ConfigHome
				if scenario == "foreign" {
					incus.Reconcile.ProjectConfig["user.subyard.data_home"] = "/foreign"
				}
			}
			program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: runtime})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			bootstrap, err := program.prepareProfileBootstrap(context.Background(), loaded, "sample", "provision", nil)
			if scenario == "missing" || scenario == "unmarked" {
				if err == nil || !strings.Contains(err.Error(), "continuation consent") {
					t.Fatalf("missing native prerequisite skipped ownership assessment: %v", err)
				}
				return
			}
			if scenario == "foreign" {
				if err == nil && bootstrap.ready != nil {
					t.Fatal("foreign project admitted to guest-only provision")
				}
				return
			}
			if err != nil || bootstrap.init != nil {
				t.Fatalf("guest-only hook requested unrelated init: %v", err)
			}
			named, err := program.prepareProvisionExecution(loaded, []string{"sample"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(named.profiles, []string{"sample"}) {
				t.Fatal("named selection expanded", named.profiles)
			}
			unnamed, err := program.prepareProvisionExecution(loaded, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(unnamed.profiles, []string{"sample", "other"}) {
				t.Fatal("unnamed selection changed", unnamed.profiles)
			}
			if err := bootstrap.refresh(context.Background(), program); err != nil {
				t.Fatalf("unchanged native prerequisites rejected: %v", err)
			}
			incus.Reconcile.ProjectConfig["user.subyard.data_home"] = "/foreign"
			if err := bootstrap.apply(context.Background(), program); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("changed project authority admitted before hook: %v", err)
			}
			incus.Reconcile.ProjectConfig["user.subyard.data_home"] = runtime.Yard.Paths.DataHome
			writeCLIFile(t, filepath.Join(root, "config/profiles/sample/profile.conf"), "YARD_DEVICES=kvm\n", 0600)
			device := map[string]string{"type": "unix-char", "source": "/dev/kvm", "path": "/dev/kvm", "mode": "0666"}
			incus.Reconcile.Instance.LocalDevices["yx-dev-kvm"] = device
			incus.Reconcile.Instance.Devices["yx-dev-kvm"] = device
			withDevice, err := program.prepareProfileBootstrap(context.Background(), loaded, "sample", "provision", nil)
			if err != nil || withDevice.ready == nil {
				t.Fatalf("matching device refused: %v", err)
			}
			if err := os.Remove(filepath.Join(runtime.HostDeviceRoot, "kvm")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/dev/zero", filepath.Join(runtime.HostDeviceRoot, "kvm")); err != nil {
				t.Fatal(err)
			}
			if err := withDevice.apply(context.Background(), program); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("replaced device source admitted before hook: %v", err)
			}
			incus.Reconcile.Instance.Config["volatile.uuid"] = "replacement"
			if err := bootstrap.refresh(context.Background(), program); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("changed native identity admitted: %v", err)
			}
		})
	}
}

func TestSelectedProvisionReusesLimitedContinuation(t *testing.T) {
	program, execution, incus, path := legacyContinuationInstalledFixture(t)
	writeConfigCommandFile(t, path, "SSH_PORT=2240\nENVIRONMENT_PROFILES=fixture-selected\n")
	delete(program.baseEnv, "ENVIRONMENT_PROFILES")
	delete(program.env, "ENVIRONMENT_PROFILES")
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	runtime := execution.platform.(reconcileruntime.Runtime)
	runtime.Environment = environmentList(loaded.Environment, nil)
	runtime.HostDeviceRoot = testkit.TempDir(t)
	canonicalLegacyInstanceFixture(runtime, incus)
	execution.loaded, execution.platform = loaded, runtime
	if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{mode: initReconcile, continueLegacy: true}); err != nil {
		t.Fatal(err)
	}
	if err := execution.legacyContinuation.publish(execution); err != nil {
		t.Fatal(err)
	}
	program.options.InitPlatform = runtime
	bootstrap, err := program.prepareProfileBootstrap(context.Background(), loaded, "fixture-selected", "provision", nil)
	if err != nil || bootstrap.ready == nil || bootstrap.init != nil {
		t.Fatalf("limited grant did not authorize the selected hook: %v", err)
	}
	if err := bootstrap.refresh(context.Background(), program); err != nil {
		t.Fatal(err)
	}
	incus.Reconcile.Instance.Config["volatile.uuid"] = "replacement"
	if err := bootstrap.apply(context.Background(), program); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("replacement passed grant revalidation: %v", err)
	}
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
		for _, scenario := range []string{"apply", "registered source", "decline", "stale", "unmigrated source appeared", "init failure", "hook failure", "no hook"} {
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
					if scenario == "unmigrated source appeared" {
						writeUnmigratedSettingsManifest(t, filepath.Join(root, "state"))
					}
					if scenario == "registered source" {
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
				if scenario != "apply" && scenario != "registered source" && scenario != "no hook" {
					want = 1
				}
				if code != want {
					t.Fatalf("code=%d want=%d stderr=%s", code, want, stderr.String())
				}
				if scenario != "decline" && (len(prompt.requests) != 1 || prompt.requests[0].Default != "yes") {
					t.Fatalf("prompts=%v", prompt.requests)
				}
				content, _ := os.ReadFile(path)
				if scenario == "decline" || scenario == "stale" || scenario == "unmigrated source appeared" {
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

func TestProvisionActivationAllowsRegisteredSource(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "sample")
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, InitPlatform: newInitPlatformFixture()})
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
	bootstrap, err := program.prepareProfileBootstrap(context.Background(), loaded, "sample", "provision", nil)
	if err != nil || bootstrap == nil || bootstrap.profiles != "sample" {
		t.Fatalf("registered source blocked local activation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "yards", "default", "config.env")); !os.IsNotExist(err) {
		t.Fatal("planning wrote selection")
	}
}

func TestProvisionActivationKeepsGitOnlyYardSettingsAsFallback(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "sample")
	configHome := filepath.Join(root, "state")
	gitPath := filepath.Join(configHome, config.GitSettingsRelativePath, "yards/demo/config.env")
	if err := os.MkdirAll(filepath.Dir(gitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "ENVIRONMENT_PROFILES=existing\nSSH_PORT=2345\n"
	writeCLIFile(t, gitPath, original, 0o600)
	if err := configsync.RegisterSource(configHome, testkit.TempDir(t)); err != nil {
		t.Fatal(err)
	}
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, InitPlatform: newInitPlatformFixture()})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("demo")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := program.prepareProfileBootstrap(context.Background(), loaded, "sample", "provision", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.apply(context.Background(), program); err != nil {
		t.Fatal(err)
	}
	local, err := os.ReadFile(filepath.Join(configHome, "yards/demo/config.env"))
	if err != nil || string(local) != "ENVIRONMENT_PROFILES='existing sample'\n" {
		t.Fatalf("profile activation copied unrelated fallback settings: %q, %v", local, err)
	}
	cached, err := os.ReadFile(gitPath)
	if err != nil || string(cached) != original {
		t.Fatalf("profile activation changed Git fallback: %q, %v", cached, err)
	}
}
