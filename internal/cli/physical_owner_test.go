package cli

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Native synthetic owner scripts exercise the exact context that reaches the
// physical boundary, including resume after a cancelled session guard.
type ownerHookAdapter struct {
	root   string
	incus  *testkit.Incus
	calls  int
	refuse bool
}

func (adapter *ownerHookAdapter) Run(ctx context.Context, request domain.AdapterRequest, _ io.Reader) (domain.AdapterResult, string, error) {
	adapter.calls++
	if request.Context["DEV_USER"] != "dev" || request.Context["DEV_UID"] != "1000" || request.Context["ENVIRONMENT_PROFILES"] != "sample" {
		return domain.AdapterResult{}, "", errors.New("physical hook received desired owner")
	}
	environment := maps.Clone(request.Context)
	environment["SUBYARD_ENGINE_CONTEXT"], environment["SUBYARD_ENGINE_CONTEXT_SCHEMA"] = "1", "1"
	if err := profile.RunServices(ctx, adapter.root, []string{"--pause"}, environment, io.Discard, io.Discard); err != nil {
		return domain.AdapterResult{}, "", err
	}
	if err := profile.RunServices(ctx, adapter.root, []string{"--resume", "sample"}, environment, io.Discard, io.Discard); err != nil {
		return domain.AdapterResult{}, "", err
	}
	if adapter.refuse {
		return domain.AdapterResult{}, "", errors.New("fixture session guard refused stop")
	}
	key := request.Context["INCUS_PROJECT"] + "/" + request.Context["YARD_INSTANCE_NAME"]
	instance := adapter.incus.Instances[key]
	instance.Status = "Stopped"
	adapter.incus.Instances[key] = instance
	return domain.AdapterResult{Schema: 1, OperationID: request.OperationID, Status: "ok"}, "", nil
}

func TestPhysicalLifecycleOwnerIsAppliedAndBoundBeforeNativeHooks(t *testing.T) {
	for _, scenario := range []string{"applied", "changed-owner", "changed-profiles", "refused-stop"} {
		t.Run(scenario, func(t *testing.T) {
			stale := scenario == "changed-owner" || scenario == "changed-profiles"
			fixture := newConfigApplyRepairFixture(t, false)
			cli := fixture.cli
			loaded, err := cli.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			loaded.Context.DevUser, loaded.Context.DevUID = "other", 1002
			loaded.Environment["DEV_USER"], loaded.Environment["DEV_UID"] = "other", "1002"
			platform := &appliedProfilesPlatformFixture{appliedSelectionPlatformFixture: &appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), settings: map[string]string{"DEV_USER": "dev", "DEV_UID": "1000"}}, profiles: []string{"sample"}, known: true}
			loaded.Environment["ENVIRONMENT_PROFILES"] = ""
			cli.options.InitPlatform = platform
			root := cli.options.RepositoryRoot
			log := filepath.Join(root, "native-owner-hooks")
			writeCLIFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), `{"schema_version":1,"owner_service":"owner.sh"}`, 0o600)
			writeCLIFile(t, filepath.Join(root, "config/profiles/sample/owner.sh"), "#!/bin/sh\n[ \"$DEV_USER:$DEV_UID:$ENVIRONMENT_PROFILES\" = dev:1000:sample ] || exit 19\nprintf '%s\\n' \"$1\" >> '"+log+"'\n", 0o700)
			runner := &ownerHookAdapter{root: root, incus: cli.options.Incus.(*testkit.Incus), refuse: scenario == "refused-stop"}
			cli.options.AdapterRunner = runner
			execution := &lifecycleExecution{action: "stop"}
			if err := cli.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
				t.Fatal(err)
			}
			if err := cli.bindLifecycleOwner(context.Background(), loaded, execution); err != nil {
				t.Fatal(err)
			}
			binding := execution.binding()
			if scenario == "changed-owner" {
				platform.settings["DEV_USER"] = "replacement"
			}
			if scenario == "changed-profiles" {
				platform.profiles = nil
			}
			orchestrator := cli.operationOrchestrator("applied-owner", loaded, nil, nil)
			plan, err := orchestrator.PlanAction(context.Background(), loaded.Context, "stop", domain.RemoteOnController, "yard.stop", domain.ActionDelta{Changed: true, Consequences: []string{"stop selected yard"}}, true)
			if err != nil {
				t.Fatal(err)
			}
			result, err := cli.executeLifecycle(context.Background(), orchestrator, loaded, plan, execution, io.Discard)
			if stale {
				if !errors.Is(err, domain.ErrPlanStale) || runner.calls != 0 || binding != execution.binding() {
					t.Fatalf("owner race crossed hooks: %v calls=%d", err, runner.calls)
				}
				return
			}
			if scenario == "refused-stop" {
				if err == nil {
					t.Fatal("session refusal was ignored")
				}
				info := runner.incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]
				if info.Status != "Running" {
					t.Fatal("refused stop changed power")
				}
			} else if err != nil || result.Status != "ok" {
				t.Fatalf("native physical owner: %#v %v", result, err)
			}
			payload, err := os.ReadFile(log)
			if err != nil || string(payload) != "--pause\n--resume\n" {
				t.Fatalf("native owner hooks: %q %v", payload, err)
			}
			if loaded.Context.DevUser != "other" || loaded.Environment["DEV_USER"] != "other" || loaded.Environment["ENVIRONMENT_PROFILES"] != "" {
				t.Fatal("physical projection changed desired configuration")
			}
		})
	}
}

func TestTeardownPhysicalOwnerPersistsAndRejectsChangedBaseline(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	cli := fixture.cli
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Context.DevUser = "other"
	loaded.Environment["DEV_USER"] = "other"
	platform := &appliedProfilesPlatformFixture{appliedSelectionPlatformFixture: &appliedSelectionPlatformFixture{initPlatformFixture: newInitPlatformFixture(), settings: map[string]string{"DEV_USER": "dev", "DEV_UID": "1000"}}, profiles: []string{"sample"}, known: true}
	loaded.Environment["ENVIRONMENT_PROFILES"] = ""
	cli.options.InitPlatform = platform
	execution := &teardownExecution{}
	execution.selectYard(loaded)
	if err := cli.bindTeardownOwner(context.Background(), loaded, execution); err != nil {
		t.Fatal(err)
	}
	values := structuredCommandContext(loaded)
	applyTeardownOwner(values, execution)
	if values["DEV_USER"] != "dev" || values["DEV_UID"] != "1000" || execution.Yards[0].Environment[teardownOwnerUser] != "dev" || values["ENVIRONMENT_PROFILES"] != "sample" || execution.Yards[0].Environment[teardownOwnerProfiles] != "sample" {
		t.Fatal("teardown owner was not persisted or projected")
	}
	before := execution.binding()
	platform.settings["DEV_USER"] = "replacement"
	if err := cli.bindTeardownOwner(context.Background(), loaded, execution); !errors.Is(err, domain.ErrPlanStale) || execution.binding() != before {
		t.Fatalf("teardown owner race: %v", err)
	}
	platform.settings["DEV_USER"] = "dev"
	platform.profiles = nil
	if err := cli.bindTeardownOwner(context.Background(), loaded, execution); !errors.Is(err, domain.ErrPlanStale) || execution.binding() != before {
		t.Fatalf("teardown profile race: %v", err)
	}
	delete(execution.activeYard.Environment, teardownOwnerProfiles)
	legacyBinding := execution.binding()
	if err := cli.bindTeardownOwner(context.Background(), loaded, execution); err != nil || execution.binding() != legacyBinding {
		t.Fatalf("legacy teardown gained unapproved profile projection: %v", err)
	}
	legacy := map[string]string{"ENVIRONMENT_PROFILES": "legacy-selection"}
	applyTeardownOwner(legacy, execution)
	if legacy["ENVIRONMENT_PROFILES"] != "legacy-selection" {
		t.Fatal("legacy teardown profile environment was overwritten")
	}
	// Existing cleanup continues once the original guest is gone.
	incus := cli.options.Incus.(*testkit.Incus)
	delete(incus.Instances, loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName)
	if err := cli.bindTeardownOwner(context.Background(), loaded, execution); err != nil {
		t.Fatal(err)
	}
}

type unreadableAppliedPlatformFixture struct{ *initPlatformFixture }

func (unreadableAppliedPlatformFixture) AppliedIntegrationConfiguration(context.Context) (reconcileruntime.AppliedIntegrationConfiguration, error) {
	return reconcileruntime.AppliedIntegrationConfiguration{}, errors.New("fixture guest is unresponsive")
}

func TestUnreadableAppliedIdentityKeepsStopAndShutdownAvailable(t *testing.T) {
	fixture := newConfigApplyRepairFixture(t, false)
	cli := fixture.cli
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	cli.options.InitPlatform = unreadableAppliedPlatformFixture{newInitPlatformFixture()}
	execution := &lifecycleExecution{action: "stop"}
	if err := cli.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
		t.Fatal(err)
	}
	if err := cli.bindLifecycleOwner(context.Background(), loaded, execution); err != nil {
		t.Fatalf("unreadable applied identity blocked stop: %v", err)
	}
	if execution.owner == nil || execution.owner.User != loaded.Context.DevUser || execution.owner.UID != loaded.Context.DevUID {
		t.Fatalf("stop owner = %#v, want desired account", execution.owner)
	}
	if shutdown := cli.withInstalledAccount(context.Background(), loaded); shutdown.Context.DevUser != loaded.Context.DevUser {
		t.Fatalf("shutdown account = %q", shutdown.Context.DevUser)
	}
}
