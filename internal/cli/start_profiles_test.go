package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestStartProfileWaitUsesAppliedOwnerAfterCommitAndPublication(t *testing.T) {
	for _, scenario := range []string{"ready", "refusal", "rename-marker", "missing-applied", "partial-applied"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, false)
			options := fixture.cli.options
			root := options.RepositoryRoot
			publication, checked := filepath.Join(root, "publication"), filepath.Join(root, "profile-checked")
			writeCLIFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), `{"schema_version":1,"startup_readiness":"startup.sh"}`, 0600)
			state := `{"state":"ready"}`
			if scenario == "refusal" {
				state = `{"state":"starting","reason":"synthetic service is reconnecting","hint":"run yard migrate to repair"}`
			}
			writeCLIFile(t, filepath.Join(root, "config/profiles/sample/startup.sh"), fmt.Sprintf(`#!/bin/sh
[ "$DEV_USER:$DEV_UID:$ENVIRONMENT_PROFILES" = dev:1000:sample ] || exit 19
[ -f %q ] || exit 20
case "$SUBYARD_STARTUP_REMAINING_MS" in ''|*[!0-9]*) exit 21 ;; esac
: > %q
printf '%%s\n' '%s'
`, publication, checked, state), 0700)
			platform := &appliedProfilesPlatformFixture{appliedSelectionPlatformFixture: &appliedSelectionPlatformFixture{
				initPlatformFixture: newInitPlatformFixture(), settings: map[string]string{"DEV_USER": "dev", "DEV_UID": "1000"}}, profiles: []string{"sample"}, known: true}
			if scenario == "partial-applied" {
				platform.settings = map[string]string{"DEV_USER": "dev"}
			}
			if scenario == "missing-applied" {
				platform.settings = nil
			}
			options.InitPlatform, options.NetworkPolicy = platform, allowTestNetworkPolicy()
			native := options.Incus.(*testkit.Incus)
			key := "subyard/yard"
			instance := native.Instances[key]
			instance.Status = "Stopped"
			instance.Config = map[string]string{
				"user.subyard.desired_power": "stopped", "user.subyard.managed": "true",
				"user.subyard.name": "default", "user.subyard.bridge": "incusbr0",
				"user.subyard.initialized": "true", "boot.autostart": "false",
				"user.subyard.deferred_publication.synthetic": `{"version":1,"device":{"listen":"tcp:192.0.2.12:8080"}}`,
			}
			native.Instances[key] = instance
			runner := &testkit.ScriptedAdapter{}
			options.AdapterRunner = runner
			program, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			loaded.Context.DevUser, loaded.Context.DevUID = "next-user", 1002
			loaded.Environment["DEV_USER"], loaded.Environment["DEV_UID"] = "next-user", "1002"
			loaded.Environment["ENVIRONMENT_PROFILES"] = ""
			if scenario == "rename-marker" {
				writeCLIFile(t, config.DeveloperIdentityMarkerPath(loaded.Context.Paths.ConfigHome, "default"), `{}`, 0600)
			}
			definition, _ := program.manifest.Lookup("start")
			prepared := &preparedCommand{CLI: program, Definition: definition, Loaded: loaded, Arguments: []string{"--yes"}}
			if err := prepared.prepareLifecycle(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if err := prepared.preparePlan(context.Background()); err != nil {
				t.Fatal(err)
			}
			ok := domain.AdapterResult{Schema: 1, OperationID: prepared.Plan.OperationID, Status: "ok"}
			runner.Steps = []testkit.AdapterStep{{Result: ok, Apply: func(domain.AdapterRequest) {
				instance := native.Instances[key]
				instance.Status = "Running"
				native.Instances[key] = instance
			}}, {Result: ok, Apply: func(request domain.AdapterRequest) {
				if native.Instances[key].Config["user.subyard.desired_power"] != "running" ||
					len(request.Arguments) != 2 || request.Arguments[1] != "--publication-retry-only" {
					t.Fatal("publication retry did not follow power commit", request)
				}
				testkit.WriteFile(t, publication, nil, 0600)
			}}}
			orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, loaded, nil, &definition)
			if _, err := prepared.Confirm(context.Background(), orchestrator, true); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			var diagnostics bytes.Buffer
			result, err := prepared.execute(ctx, orchestrator, &diagnostics)
			if scenario == "refusal" {
				var refusal *profile.StartupRefusal
				if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "yard started (desired=running)") || refusal.Reason != "synthetic service is reconnecting" || !strings.Contains(refusal.Hint, "yard migrate") {
					t.Fatalf("lost readiness refusal: %v", err)
				}
			} else if err != nil || result.Status != "ok" {
				t.Fatalf("start: %+v %v", result, err)
			}
			_, probeErr := os.Stat(checked)
			if (probeErr == nil) != (scenario == "ready" || scenario == "refusal") {
				t.Fatalf("unexpected profile probe in %s: %v", scenario, probeErr)
			}
			if actual := native.Instances[key]; actual.Status != "Running" || actual.Config["user.subyard.desired_power"] != "running" || len(runner.Requests) != 2 {
				t.Fatal("profile refusal lost committed power or publication retry", actual, runner.Requests)
			}
		})
	}
}

func TestStartProfileReadinessBudgetExcludesSlowPhysicalBoot(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	options := fixture.cli.options
	root := options.RepositoryRoot
	checked := filepath.Join(root, "slow-boot-profile-checked")
	writeCLIFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), `{"schema_version":1,"startup_readiness":"startup.sh"}`, 0600)
	writeCLIFile(t, filepath.Join(root, "config/profiles/sample/startup.sh"), fmt.Sprintf(`#!/bin/sh
[ "$SUBYARD_STARTUP_REMAINING_MS" -gt 25000 ] || exit 19
: > %q
printf '%%s\n' '{"state":"ready"}'
`, checked), 0700)
	options.InitPlatform = &appliedProfilesPlatformFixture{appliedSelectionPlatformFixture: &appliedSelectionPlatformFixture{
		initPlatformFixture: newInitPlatformFixture(), settings: map[string]string{"DEV_USER": "dev", "DEV_UID": "1000"}}, profiles: []string{"sample"}, known: true}
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := program.waitForStartedProfiles(context.Background(), loaded, time.Now().Add(-10*time.Minute), &bytes.Buffer{}); err != nil {
		t.Fatalf("physical boot consumed the profile readiness budget: %v", err)
	}
	if _, err := os.Stat(checked); err != nil {
		t.Fatal("applied profile was not observed", err)
	}
}

func TestLifecycleStartAnchorPrecedesPhysicalEffects(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	native := lifecycleIncus()
	runner := &testkit.ScriptedAdapter{}
	program, err := New(Options{RepositoryRoot: root, WorkingDir: root, Environment: environment,
		Incus: native, AdapterRunner: runner, NetworkPolicy: allowTestNetworkPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	execution := &lifecycleExecution{action: "start"}
	if err := program.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
		t.Fatal(err)
	}
	orchestrator := program.operationOrchestrator("start-anchor", loaded, nil, nil)
	plan, err := orchestrator.PlanAction(context.Background(), loaded.Context, "start", domain.RemoteOnController,
		"yard.start", domain.ActionDelta{Changed: true, Consequences: []string{"start selected yard"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	var captured time.Time
	before := time.Now()
	runner.Steps = []testkit.AdapterStep{{Result: domain.AdapterResult{Schema: 1, OperationID: plan.OperationID, Status: "ok"}, Apply: func(domain.AdapterRequest) {
		captured = execution.started
		if captured.IsZero() || captured.Before(before) || captured.After(time.Now()) {
			t.Fatal("physical start has no invocation anchor")
		}
		instance := native.Instances["subyard/yard"]
		instance.Status = "Running"
		native.Instances["subyard/yard"] = instance
	}}}
	if _, err := program.executeLifecycle(context.Background(), orchestrator, loaded, plan, execution, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if execution.started != captured {
		t.Fatal("physical startup anchor was reset after the adapter")
	}
}

func TestIdentityInspectionPhysicalStartNeverProbesDesiredAccount(t *testing.T) {
	for _, marker := range []bool{false, true} {
		t.Run(fmt.Sprint(marker), func(t *testing.T) {
			root := testkit.TempDir(t)
			scripts := filepath.Join(root, "scripts")
			if err := os.MkdirAll(filepath.Join(scripts, "lib"), 0700); err != nil {
				t.Fatal(err)
			}
			guard, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts/lifecycle-guard.sh"))
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(scripts, "lifecycle-guard.sh"), guard, 0700)
			for name, body := range map[string]string{
				"lib/engine-context.sh": "subyard_require_engine_context() { :; }",
				"lib/ui.sh":             "info() { :; }; die() { exit 1; }",
				"lib-power.sh":          "power_nm_prepare_reader() { :; }; power_start_guarded() { :; }",
				"lib/host.sh":           "incus_preflight() { :; }; incus_wait_yard_agent() { :; }",
				"lib/ssh-listener.sh":   "ssh_listener_restore() { :; }",
				"profile-services.sh":   "#!/bin/sh\nexit 91\n",
			} {
				testkit.WriteFile(t, filepath.Join(scripts, name), []byte(body+"\n"), 0700)
			}
			testkit.WriteFile(t, filepath.Join(root, "incus"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			yard := domain.Context{YardName: "default", IncusProject: "project", YardInstanceName: "yard", DevUser: "not-created-yet", DevUID: 1002}
			if marker {
				testkit.WriteFile(t, config.DeveloperIdentityMarkerPath(root, "default"), []byte(`{}`), 0600)
			}
			before := ports.InstanceInfo{Name: "yard", Status: "Stopped", Config: map[string]string{"volatile.uuid": "same"}}
			native := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"project/yard": before}}
			power := application.PowerService{Instances: native, Config: native}
			_, err = startIdentityInspectionPower(context.Background(), power, yard, before, func() error {
				command := exec.Command("bash", filepath.Join(scripts, "lifecycle-guard.sh"), "start", "--reconcile")
				command.Env = environmentList(environmentMap(os.Environ()), map[string]string{"PATH": root + ":" + os.Getenv("PATH"), "DEV_USER": yard.DevUser, "VM_CPU_WEIGHT": ""})
				return command.Run()
			})
			if err != nil {
				t.Fatal("inspection boot was blocked by profile readiness", err)
			}
		})
	}
}
