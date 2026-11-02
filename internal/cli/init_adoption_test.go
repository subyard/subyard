package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

type adoptionIncus struct {
	*testkit.Incus
	writes     int
	afterWrite func()
}

func (fake *adoptionIncus) AdoptProject(_ context.Context, _ string, before, metadata map[string]string) error {
	if !maps.Equal(before, fake.Reconcile.ProjectConfig) {
		return domain.ErrPlanStale
	}
	fake.writes++
	fake.Reconcile.ProjectConfig = maps.Clone(before)
	for key, value := range metadata {
		fake.Reconcile.ProjectConfig[key] = value
	}
	if fake.afterWrite != nil {
		fake.afterWrite()
	}
	return nil
}
func adoptionFixture(t *testing.T) (*CLI, *initExecution, *adoptionIncus) {
	t.Helper()
	program, execution, incus, _ := legacyContinuationInstalledFixture(t)
	fake := &adoptionIncus{Incus: incus}
	runtime := execution.platform.(reconcileruntime.Runtime)
	runtime.Environment = environmentList(structuredCommandContext(execution.loaded), nil)
	// Incus isolates these namespaces by default on project creation.
	fake.Reconcile.ProjectConfig["features.storage.buckets"] = "true"
	fake.Reconcile.ProjectConfig["features.images"] = "true"
	fake.Reconcile.ProjectConfig["features.profiles"] = "true"
	fake.Reconcile.ProjectConfig["features.storage.volumes"] = "true"
	runtime.SourceReader = func(config.MaterializedAsset) ([]byte, error) {
		return os.ReadFile("../../scripts/lib/project-config.sh")
	}
	runtime.Incus = fake
	execution.platform = runtime
	program.options.Incus = fake
	return program, execution, fake
}

func canonicalAdoptionVMFixture(runtime *reconcileruntime.Runtime, instance *ports.InstanceInfo) {
	runtime.Yard.YardKind, instance.Type = domain.YardVM, domain.YardVM
	runtime.Environment = append(runtime.Environment, "YARD_KIND=vm")
	for key := range instance.Config {
		if key == "security.nesting" || strings.HasPrefix(key, "volatile.idmap.") {
			delete(instance.Config, key)
			delete(instance.LocalConfig, key)
		}
	}
	delete(instance.LocalDevices, "ssh")
	delete(instance.Devices, "ssh")
}

func TestYardAdoptionPublishesOnlyMetadataAndRetiresExactContinuation(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "stopped"}[stopped], func(t *testing.T) {
			program, execution, fake := adoptionFixture(t)
			if stopped {
				fake.Reconcile.Instance.Status = "Stopped"
				fake.Reconcile.Instance.Config["user.subyard.desired_power"] = "stopped"
				fake.Reconcile.Instance.LocalConfig["user.subyard.desired_power"] = "stopped"
			}
			if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{continueLegacy: true}); err != nil {
				t.Fatal(err)
			}
			if err := execution.legacyContinuation.publish(execution); err != nil {
				t.Fatal(err)
			}
			execution.legacyContinuation = nil
			runtime := execution.platform.(reconcileruntime.Runtime)
			runtime.LegacyContinuation = nil
			execution.platform = runtime
			before := fake.Reconcile.Instance
			if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{adopt: true}); err != nil {
				t.Fatal(err)
			}
			_, delta, err := execution.actionPlan()
			if err != nil || !delta.Changed || execution.operationSteps()[0].ID != "init.adopt" || fake.writes != 0 {
				t.Fatal("adoption not assessed read-only", err)
			}
			if err := execution.legacyContinuation.applyAdoption(context.Background(), program, execution); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, fake.Reconcile.Instance) || fake.writes != 1 || len(fake.ConfigUpdates) != 0 {
				t.Fatal("adoption changed workload or power")
			}
			if fake.Reconcile.ProjectConfig["user.subyard.installation"] != domain.AdoptedInstallation || fake.Reconcile.ProjectConfig["user.subyard.owner_create"] != "" {
				t.Fatal("adoption invented creation ownership")
			}
			snapshot, err := readLegacyContinuation(execution.loaded.Context)
			if err != nil || snapshot.Exists {
				t.Fatal("continuation survived verified adoption", err)
			}
			execution.legacyContinuation = nil
			if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{}); err != nil || execution.legacyContinuation != nil {
				t.Fatal("normal init still needs continuation", err)
			}
		})
	}
}

func TestYardAdoptionRejectsDriftAndRecoversAfterPublication(t *testing.T) {
	for _, scenario := range []string{"drift", "retirement-interrupted", "retirement-replaced", "foreign-metadata", "snapshot", "bucket", "feature-namespace", "shared-profile", "shared-volume", "bucket-alias", "image-alias", "invalid-feature"} {
		t.Run(scenario, func(t *testing.T) {
			program, execution, fake := adoptionFixture(t)
			for scenarioName, feature := range map[string]struct{ key, value string }{"bucket-alias": {"features.storage.buckets", "1"}, "image-alias": {"features.images", "yes"}, "invalid-feature": {"features.storage.buckets", "unexpected"}} {
				if scenario == scenarioName {
					fake.Reconcile.ProjectConfig[feature.key] = feature.value
				}
			}
			if scenario == "shared-profile" {
				delete(fake.Reconcile.ProjectConfig, "features.profiles")
			}
			if scenario == "shared-volume" {
				delete(fake.Reconcile.ProjectConfig, "features.storage.volumes")
			}
			if strings.HasPrefix(scenario, "retirement-") {
				if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{continueLegacy: true}); err != nil {
					t.Fatal(err)
				}
				if err := execution.legacyContinuation.publish(execution); err != nil {
					t.Fatal(err)
				}
				execution.legacyContinuation = nil
				runtime := execution.platform.(reconcileruntime.Runtime)
				runtime.LegacyContinuation = nil
				execution.platform = runtime
			}
			if scenario == "foreign-metadata" {
				fake.Reconcile.ProjectConfig["user.subyard.operator_uid"] = "foreign"
			}
			if scenario == "snapshot" {
				fake.TeardownResources[2].SnapshotCount = 1
			}
			if scenario == "bucket" {
				fake.TeardownResources = append(fake.TeardownResources, ports.TeardownResource{Kind: "bucket", Name: "unproven", Pool: "default", Binding: "bucket"})
			}
			if scenario == "feature-namespace" {
				fake.Reconcile.ProjectConfig["features.networks"] = "true"
			}
			err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{adopt: true})
			if scenario != "drift" && !strings.HasPrefix(scenario, "retirement-") {
				if err == nil || fake.writes != 0 {
					t.Fatal("unsafe yard admitted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "retirement-replaced" {
				before := execution.legacyContinuation.before
				fake.afterWrite = func() {
					if err := config.CompareAndSwapLocalSettingsFile(execution.loaded.Context.Paths.ConfigHome, execution.legacyContinuation.path, before, before.Content); err != nil {
						t.Fatal(err)
					}
				}
				if err := execution.legacyContinuation.applyAdoption(context.Background(), program, execution); err == nil {
					t.Fatal("retired concurrent record replacement")
				}
				fake.afterWrite = nil
				execution.legacyContinuation = nil
				if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{adopt: true}); err != nil {
					t.Fatal(err)
				}
				if err := execution.legacyContinuation.applyAdoption(context.Background(), program, execution); err != nil {
					t.Fatal(err)
				}
				return
			}
			if scenario == "drift" {
				fake.Reconcile.Instance.Config["volatile.uuid"] = "replacement"
				if err := execution.legacyContinuation.applyAdoption(context.Background(), program, execution); !errors.Is(err, domain.ErrPlanStale) || fake.writes != 0 {
					t.Fatal("post-consent drift wrote metadata", err)
				}
				return
			}
			// Publish atomically, then interrupt before continuation retirement.
			if err := fake.AdoptProject(context.Background(), "", execution.legacyContinuation.projectBefore, execution.legacyContinuation.adoptionConfig); err != nil {
				t.Fatal(err)
			}
			execution.legacyContinuation = nil
			if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{adopt: true}); err != nil {
				t.Fatal("published adoption cannot recover", err)
			}
			if err := execution.legacyContinuation.applyAdoption(context.Background(), program, execution); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestYardAdoptionRequiresExplicitLocalScope(t *testing.T) {
	for _, args := range [][]string{{"--adopt", "--continue-legacy"}, {"--adopt", "--reset"}, {"--adopt", "--configs"}, {"--adopt", "--profile", "sample"}, {"--adopt", "--adopt"}} {
		if _, err := parseInitArguments(args); err == nil {
			t.Fatal("expanded adoption parsed", args)
		}
	}
	program, execution, fake := adoptionFixture(t)
	if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{}); err == nil || !strings.Contains(err.Error(), "--continue-legacy") || fake.writes != 0 {
		t.Fatal("ordinary init expanded authority", err)
	}
	if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{adopt: true}); err != nil {
		t.Fatal(err)
	}
	before, err := config.ReadPersistentFileSnapshot(execution.loaded.Context.Paths.ConfigHome, execution.legacyContinuation.registrationPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.CompareAndSwapLocalSettingsFile(execution.loaded.Context.Paths.ConfigHome, execution.legacyContinuation.registrationPath, before, append(before.Content, []byte("\n# drift\n")...)); err != nil {
		t.Fatal(err)
	}
	if err := execution.legacyContinuation.applyAdoption(context.Background(), program, execution); !errors.Is(err, domain.ErrPlanStale) || fake.writes != 0 {
		t.Fatal("changed protected registration admitted", err)
	}
}

var _ ports.ProjectAdopter = (*adoptionIncus)(nil)

func TestYardAdoptionKeepsContinuationVMMetadataClosed(t *testing.T) {
	for _, key := range []string{"volatile.vsock_id", "volatile.vm.definition"} {
		t.Run(key, func(t *testing.T) {
			runtime, fake := legacyContinuationFixture(t)
			fake.Reconcile.Instance.Config[key] = "native-runtime"
			fake.Reconcile.Instance.LocalConfig[key] = "native-runtime"
			if _, _, err := runtime.InspectLegacyContinuation(context.Background()); err == nil {
				t.Fatal("continuation admitted VM-only metadata on a container")
			}
			if len(fake.ConfigUpdates) != 0 || len(fake.PowerUpdates) != 0 {
				t.Fatal("continuation refusal changed native state")
			}
		})
	}
}

func TestYardAdoptionRetainsValidatedContinuationDuringVerification(t *testing.T) {
	for _, scenario := range []string{"existing-grant", "published-retry", "fresh-refused"} {
		t.Run(scenario, func(t *testing.T) {
			program, execution, fake := adoptionFixture(t)
			if scenario != "fresh-refused" {
				if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{continueLegacy: true}); err != nil {
					t.Fatal(err)
				}
				if err := execution.legacyContinuation.publish(execution); err != nil {
					t.Fatal(err)
				}
			}
			fake.Reconcile.Instance.Config["user.subyard.initialized"] = "false"
			fake.Reconcile.Instance.LocalConfig["user.subyard.initialized"] = "false"
			runtime := execution.platform.(reconcileruntime.Runtime)
			runtime.LegacyContinuation = nil
			execution.platform, execution.legacyContinuation = runtime, nil
			err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{adopt: true})
			if scenario == "fresh-refused" {
				if err == nil || fake.writes != 0 {
					t.Fatal("fresh adoption bypassed initialized identity guard", err)
				}
				return
			}
			if err != nil {
				t.Fatal("valid continuation assessment failed", err)
			}
			if scenario == "published-retry" {
				if err := fake.AdoptProject(context.Background(), runtime.Yard.IncusProject, execution.legacyContinuation.projectBefore, execution.legacyContinuation.adoptionConfig); err != nil {
					t.Fatal(err)
				}
				execution.legacyContinuation = nil
				if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{adopt: true}); err != nil {
					t.Fatal("published adoption cannot recover", err)
				}
			}
			if err := execution.legacyContinuation.applyAdoption(context.Background(), program, execution); err != nil {
				t.Fatal("verified continuation was lost after publication", err)
			}
			record, err := readLegacyContinuation(execution.loaded.Context)
			if err != nil || record.Exists {
				t.Fatal("verified adoption did not retire continuation", err)
			}
			if fake.Reconcile.Instance.Config["user.subyard.initialized"] != "false" || fake.Reconcile.Instance.LocalConfig["user.subyard.initialized"] != "false" || len(fake.PowerUpdates) != 0 || len(fake.ConfigUpdates) != 0 {
				t.Fatal("adoption changed lifecycle or workload state")
			}
		})
	}
}

type adoptionHookExecutor struct{}

func (adoptionHookExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) == 0 || request.Command[0] != "bash" || request.Command[len(request.Command)-1] != "--observe" {
		return (activationIntegrationExecutor{}).Exec(ctx, project, instance, request)
	}
	return ports.InstanceExecResult{Stdout: []byte(`{"projects":"` + strings.Repeat("1", 64) + `","wiring":"` + strings.Repeat("2", 64) + `","hooks":[],"roots":[],"facts":{},"resolved":{}}`)}, nil
}

func TestYardAdoptionPreparedCommandRequiresConsent(t *testing.T) {
	for _, scenario := range []string{"decline", "accept", "stopped", "partial-retry", "repeat", "skip-drift", "stale-native"} {
		t.Run(scenario, func(t *testing.T) {
			program, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			runtime.Executor = adoptionHookExecutor{}
			fake.ServerInfo.Version = "6.0.7"
			fake.Reconcile.HostPoolFound, fake.Reconcile.HostNetworkFound = true, true
			runtime.ACLToolsAvailable = func() (bool, error) { return true, nil }
			program.options.InitPlatform = runtime
			// This test owns init composition; release readiness has separate native
			// and CLI contracts, while the synthetic installed runtime has drift.
			program.releaseTransitionChild = true
			normal := newInitPlatformFixture()
			normal.converged[ports.ReconcileStageExtras] = false
			if scenario == "stopped" {
				fake.Reconcile.Instance.Status = "Stopped"
				fake.Reconcile.Instance.Config["user.subyard.desired_power"] = "stopped"
				fake.Reconcile.Instance.LocalConfig["user.subyard.desired_power"] = "stopped"
			}
			before := operationStateDigest(fake.Reconcile.Instance)
			var output bytes.Buffer
			program.options.Stdout, program.options.Stderr = &output, &output
			definition, _ := program.manifest.Lookup("init")
			prepare := func() *preparedCommand {
				if fake.writes > 0 {
					// Keep later physical checks conditional in this synthetic runtime;
					// the native fixture owns their actual installed adapters.
					fake.Reconcile.HostPoolFound = false
				}
				prepared := &preparedCommand{lifecycle: &preparedLifecycle{}, CLI: program, Definition: definition, Loaded: execution.loaded, Arguments: []string{"--adopt"}}
				prepared.policy = commandPolicy(definition, execution.loaded.Context, nil, nil)
				if err := prepared.prepareInit(context.Background(), nil); err != nil {
					t.Fatal(err)
				}
				fake.afterWrite = func() {
					normal.converged[ports.ReconcileStageProject] = true
					prepared.initExecution.platform = normal
					if scenario == "skip-drift" {
						normal.converged[ports.ReconcileStageIncus] = false
					}
					if scenario == "partial-retry" && fake.writes == 1 {
						fake.Reconcile.Instance.Config["user.subyard.initialized"] = "false"
						fake.Reconcile.Instance.LocalConfig["user.subyard.initialized"] = "false"
					}
				}
				if err := prepared.preparePlan(context.Background()); err != nil {
					t.Fatal(err)
				}
				steps := prepared.initExecution.operationSteps()
				if steps[0].ID != "init.adopt" || !slices.ContainsFunc(steps, func(step domain.OperationStep) bool { return step.ID == "init.stage.extras" }) || steps[len(steps)-1].ID != "init.finalize" {
					t.Fatal("combined effects absent from initial operation plan", steps)
				}
				if slices.ContainsFunc(steps, func(step domain.OperationStep) bool { return step.ID == "init.legacy-continuation" }) {
					t.Fatal("adoption would publish a continuation grant")
				}
				return prepared
			}
			prepared := prepare()
			if scenario == "partial-retry" {
				normal.applyErr = errors.New("injected extras failure")
			}
			if scenario == "stale-native" {
				fake.Reconcile.Instance.Config["volatile.uuid"] = "replacement"
			}
			result := program.runPreparedCommand(context.Background(), prepared, scenario != "decline")
			switch scenario {
			case "decline", "stale-native":
				if result == 0 || fake.writes != 0 || len(normal.applied) != 0 || normal.projectHooks != 0 {
					t.Fatal("unapproved init mutated", result, output.String())
				}
				return
			case "partial-retry", "skip-drift":
				if result == 0 || fake.writes != 1 || !strings.Contains(output.String(), "registration is complete; initialization did not finish; rerun yard init --adopt") {
					t.Fatal("partial failure lost truthful retry", result, output.String())
				}
				if scenario == "skip-drift" {
					if len(normal.applied) != 0 {
						t.Fatal("unapproved skip expansion applied work")
					}
					return
				}
				normal.applyErr = nil
				output.Reset()
				prepared = prepare()
				result = program.runPreparedCommand(context.Background(), prepared, true)
			case "repeat":
				if result != 0 {
					t.Fatal("first init failed", output.String())
				}
				prepared = prepare()
				result = program.runPreparedCommand(context.Background(), prepared, true)
			}
			if result != 0 || !slices.Contains(normal.applied, ports.ReconcileStageExtras) || normal.projectHooks == 0 || !slices.Contains(normal.applied, ports.ReconcileStageFinalize) {
				t.Fatal("combined adoption did not reconcile", result, output.String())
			}
			if fake.Reconcile.ProjectConfig["user.subyard.installation"] != domain.AdoptedInstallation {
				t.Fatal("registration lost")
			}
			if scenario != "partial-retry" && operationStateDigest(fake.Reconcile.Instance) != before {
				t.Fatal("init changed native identity or power in converged fixture")
			}
			if len(fake.PowerUpdates) != 0 {
				t.Fatal("adoption changed desired power")
			}
			snapshot, err := readLegacyContinuation(execution.loaded.Context)
			if err != nil || snapshot.Exists {
				t.Fatal("adoption manufactured continuation", err)
			}
		})
	}
}

func TestYardAdoptionAdmitsConfiguredNativeKinds(t *testing.T) {
	for _, kind := range []string{"nested", "no-host", "vm", "base-image", "foreign-image", "duplicate-image"} {
		t.Run(kind, func(t *testing.T) {
			_, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			instance := &fake.Reconcile.Instance
			if strings.Contains(kind, "image") {
				instance.Config["volatile.base_image"] = strings.Repeat("a", 64)
				instance.LocalConfig["volatile.base_image"] = strings.Repeat("a", 64)
				fake.Reconcile.ProjectConfig["features.images"] = "true"
				image := ports.TeardownResource{Kind: "image", Name: strings.Repeat("a", 64), Binding: "image-cache"}
				if kind == "foreign-image" {
					image.Name = strings.Repeat("b", 64)
				}
				fake.TeardownResources = append(fake.TeardownResources, image)
				if kind == "duplicate-image" {
					fake.TeardownResources = append(fake.TeardownResources, image)
				}
			}
			if kind == "nested" {
				runtime.Yard.NestedE2EVMs = true
				runtime.Environment = append(runtime.Environment, "NESTED_E2E_VMS=1")
				fake.Reconcile.ProjectConfig["restricted.containers.interception"] = "allow"
				for _, key := range []string{"security.syscalls.intercept.bpf", "security.syscalls.intercept.bpf.devices"} {
					instance.Config[key] = "true"
					instance.LocalConfig[key] = "true"
				}
				for name, source := range map[string]string{"e2e-vsock": "/dev/vsock", "e2e-vhost-vsock": "/dev/vhost-vsock", "e2e-tun": "/dev/net/tun"} {
					device := map[string]string{"type": "unix-char", "source": source, "path": source}
					instance.LocalDevices[name] = device
					instance.Devices[name] = maps.Clone(device)
				}
			}
			if kind == "no-host" {
				runtime.Environment = append(runtime.Environment, "ALLOWS_HOST_ACCESS=false", "HOST_MOUNTS=")
				for name := range instance.LocalDevices {
					if name != "srv" && name != "ssh" {
						delete(instance.LocalDevices, name)
						delete(instance.Devices, name)
					}
				}
			}
			if kind == "vm" {
				canonicalAdoptionVMFixture(&runtime, instance)
				instance.Config["volatile.vsock_id"] = "42"
				instance.LocalConfig["volatile.vsock_id"] = "42"
			}
			_, _, _, _, err := runtime.InspectYardAdoption(context.Background())
			if kind == "foreign-image" || kind == "duplicate-image" {
				if err == nil {
					t.Fatal("unproven image cache admitted")
				}
				return
			}
			if err != nil {
				t.Fatal("configured native shape refused", err)
			}
		})
	}
}

func TestYardAdoptionVMVerifiedDiskBusMetadata(t *testing.T) {
	for _, scenario := range []string{"9p", "virtiofs", "stopped", "unknown-device", "undeclared-disk", "wrong-type", "local-disk-any-state", "profile-disk", "profile-invalid-bus", "container-local-disk", "container-profile-disk", "legacy", "disk-policy-drift"} {
		t.Run(scenario, func(t *testing.T) {
			_, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			runtime.Environment = append(runtime.Environment, "HOST_MOUNTS=fixture-mount:/mnt/fixture:ro:0755")
			canonicalLegacyInstanceFixture(runtime, fake.Incus)
			instance := &fake.Reconcile.Instance
			if !strings.HasPrefix(scenario, "container") && scenario != "legacy" {
				canonicalAdoptionVMFixture(&runtime, instance)
			}
			key, value := "volatile.fixture-mount.io.bus", "9p"
			switch scenario {
			case "virtiofs":
				value = "virtiofs"
			case "stopped":
				instance.Status = "Stopped"
				instance.Config["user.subyard.desired_power"] = "stopped"
				instance.LocalConfig["user.subyard.desired_power"] = "stopped"
			case "unknown-device":
				key = "volatile.unknown.io.bus"
			case "undeclared-disk":
				key = "volatile.unknown.io.bus"
				instance.LocalDevices["unknown"] = maps.Clone(instance.LocalDevices["fixture-mount"])
				instance.Devices["unknown"] = maps.Clone(instance.LocalDevices["unknown"])
			case "wrong-type":
				key = "volatile.eth0.io.bus"
			case "local-disk-any-state":
				key, value = key+".extra", "unrecognized"
			case "profile-disk", "container-profile-disk":
				key = "volatile.root.io.bus"
			case "profile-invalid-bus":
				key, value = "volatile.root.io.bus", "unrecognized"
			case "disk-policy-drift":
				instance.LocalDevices["fixture-mount"]["readonly"] = "false"
				instance.Devices["fixture-mount"]["readonly"] = "false"
			}
			instance.Config[key], instance.LocalConfig[key] = value, value
			if scenario == "legacy" {
				if _, _, err := runtime.InspectLegacyContinuation(context.Background()); err == nil {
					t.Fatal("legacy continuation admitted VM disk metadata")
				}
				return
			}
			state, _, _, _, err := runtime.InspectYardAdoption(context.Background())
			// Volatile state of a local device is taken as found. Only a VM profile disk
			// has a resolved bus here, and its value stays checked.
			if scenario == "unknown-device" || scenario == "wrong-type" || scenario == "profile-invalid-bus" || scenario == "container-profile-disk" {
				if err == nil {
					t.Fatal("unverified disk bus metadata admitted")
				}
				return
			}
			if err != nil {
				t.Fatal("verified native VM disk metadata refused", err)
			}
			before, err := runtime.LegacyContinuationSecurityBinding(state)
			if err != nil {
				t.Fatal(err)
			}
			instance.Config[key], instance.LocalConfig[key] = "virtiofs", "virtiofs"
			if value == "virtiofs" {
				instance.Config[key], instance.LocalConfig[key] = "9p", "9p"
			}
			state, _, _, _, err = runtime.InspectYardAdoption(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			after, err := runtime.LegacyContinuationSecurityBinding(state)
			if err != nil || before == after {
				t.Fatal("native disk bus drift escaped exact security binding", err)
			}
		})
	}
}

func TestYardAdoptionVMRuntimeMetadata(t *testing.T) {
	const adjustment, offset, reset = "volatile.vm.rtc_adjustment", "volatile.vm.rtc_offset", "volatile.vm.needs_reset"
	for _, test := range []struct {
		name                     string
		values                   map[string]string
		allow, container, legacy bool
	}{
		{name: "absent", allow: true},
		{name: "zero", values: map[string]string{adjustment: "0", offset: "0"}, allow: true},
		{name: "negative", values: map[string]string{adjustment: "-42", offset: "42"}, allow: true},
		{name: "missing-offset", values: map[string]string{adjustment: "0"}, allow: true},
		{name: "reset-true", values: map[string]string{reset: "true"}, allow: true},
		{name: "overflow", values: map[string]string{offset: "9223372036854775808"}},
		{name: "invalid-rtc", values: map[string]string{adjustment: "true"}},
		{name: "noncanonical-rtc", values: map[string]string{offset: "01"}},
		{name: "reset-false", values: map[string]string{reset: "false"}},
		{name: "reset-empty", values: map[string]string{reset: ""}},
		{name: "rtc-suffix", values: map[string]string{offset + ".extra": "0"}},
		{name: "reset-suffix", values: map[string]string{reset + ".extra": "true"}},
		{name: "container-rtc", values: map[string]string{adjustment: "0", offset: "0"}, container: true},
		{name: "container-reset", values: map[string]string{reset: "true"}, container: true},
		{name: "legacy-rtc", values: map[string]string{adjustment: "0", offset: "0"}, container: true, legacy: true},
		{name: "legacy-reset", values: map[string]string{reset: "true"}, container: true, legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			instance := &fake.Reconcile.Instance
			if !test.container {
				canonicalAdoptionVMFixture(&runtime, instance)
			}
			for key, value := range test.values {
				instance.Config[key], instance.LocalConfig[key] = value, value
			}
			if test.legacy {
				if _, _, err := runtime.InspectLegacyContinuation(context.Background()); err == nil {
					t.Fatal("legacy continuation admitted VM runtime metadata")
				}
				return
			}
			state, _, _, _, err := runtime.InspectYardAdoption(context.Background())
			if !test.allow {
				if err == nil {
					t.Fatal("invalid VM runtime metadata admitted")
				}
				return
			}
			if err != nil {
				t.Fatal("native VM runtime metadata refused", err)
			}
			before, err := runtime.LegacyContinuationSecurityBinding(state)
			if err != nil {
				t.Fatal(err)
			}
			instance.Config[offset], instance.LocalConfig[offset] = "1", "1"
			state, _, _, _, err = runtime.InspectYardAdoption(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			after, err := runtime.LegacyContinuationSecurityBinding(state)
			if err != nil || before == after {
				t.Fatal("VM RTC drift escaped exact security binding", err)
			}
		})
	}
}

func TestYardAdoptionSSHNativeBoundary(t *testing.T) {
	for _, scenario := range []string{"vm-relay", "vm-proxy", "container-proxy", "container-missing", "container-modified", "legacy-proxy", "legacy-missing", "legacy-modified"} {
		t.Run(scenario, func(t *testing.T) {
			_, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			instance := &fake.Reconcile.Instance
			proxy := maps.Clone(instance.LocalDevices["ssh"])
			if strings.HasPrefix(scenario, "vm-") {
				canonicalAdoptionVMFixture(&runtime, instance)
			}
			if scenario == "vm-proxy" {
				instance.LocalDevices["ssh"], instance.Devices["ssh"] = proxy, maps.Clone(proxy)
			}
			if strings.HasSuffix(scenario, "missing") {
				delete(instance.LocalDevices, "ssh")
				delete(instance.Devices, "ssh")
			}
			if strings.HasSuffix(scenario, "modified") {
				instance.LocalDevices["ssh"]["connect"] = "tcp:127.0.0.1:23"
				instance.Devices["ssh"]["connect"] = "tcp:127.0.0.1:23"
			}
			var err error
			if strings.HasPrefix(scenario, "legacy-") {
				_, _, err = runtime.InspectLegacyContinuation(context.Background())
			} else {
				_, _, _, _, err = runtime.InspectYardAdoption(context.Background())
			}
			// Adoption retains the transport as found and init converges it; only
			// non-adopt continuation still requires its exact closed shape.
			allow := !strings.HasPrefix(scenario, "legacy-") || scenario == "legacy-proxy"
			if allow && err != nil {
				t.Fatal("normal transport shape refused", err)
			}
			if !allow && err == nil {
				t.Fatal("unexpected SSH proxy boundary admitted")
			}
		})
	}
}

func TestYardAdoptionRegisteredBindDevices(t *testing.T) {
	for _, scenario := range []string{"legacy", "current", "unregistered", "other-yard", "sync", "source", "path", "options", "expanded", "inherited", "collision", "symlink", "malformed", "unsafe-directory", "stale-record"} {
		t.Run(scenario, func(t *testing.T) {
			program, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			record := domain.ProjectRecord{Schema: 1, ProjectID: "Demo.Project", Name: "Demo", HostPath: "/host/project", YardPath: "/srv/workspaces/Demo.Project/src", Mode: domain.ProjectBind, SSHHost: runtime.Yard.SSHHost}
			if scenario == "current" {
				record.IdentityVersion = 2
				record.Name = record.ProjectID
			}
			if scenario == "other-yard" {
				record.SSHHost = "other-yard"
			}
			if scenario == "sync" {
				record.Mode = domain.ProjectSync
			}
			directory := runtime.Yard.Paths.StateDir
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, record.ProjectID+".json")
			payload, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, payload, 0600)
			if scenario == "unregistered" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink" {
				target := filepath.Join(testkit.TempDir(t), "record")
				testkit.WriteFile(t, target, payload, 0600)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "malformed" {
				testkit.WriteFile(t, path, []byte("{}"), 0600)
			}
			if scenario == "unsafe-directory" {
				if err := os.Chmod(directory, 0777); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "collision" {
				other := record
				other.ProjectID = "Demo_Project"
				other.Name = "Other"
				other.YardPath = "/srv/workspaces/Demo_Project/src"
				data, _ := json.Marshal(other)
				testkit.WriteFile(t, filepath.Join(directory, other.ProjectID+".json"), data, 0600)
			}
			name := state.WorkspaceDeviceFor(record)
			device := map[string]string{"type": "disk", "source": record.HostPath, "path": record.YardPath, "shift": "true"}
			switch scenario {
			case "source":
				device["source"] = "/host/other"
			case "path":
				device["path"] = "/other"
			case "options":
				device["readonly"] = "true"
			}
			fake.Reconcile.Instance.LocalDevices[name] = device
			fake.Reconcile.Instance.Devices[name] = maps.Clone(device)
			if scenario == "expanded" {
				fake.Reconcile.Instance.Devices[name]["source"] = "/other"
			}
			if scenario == "inherited" {
				delete(fake.Reconcile.Instance.LocalDevices, name)
			}
			before := operationStateDigest(fake.Reconcile.Instance)
			err = program.prepareLegacyContinuation(context.Background(), execution, initArguments{adopt: true})
			// Registry integrity and inconsistent effective devices still refuse.
			refused := slices.Contains([]string{"expanded", "inherited", "collision", "symlink", "malformed", "unsafe-directory"}, scenario)
			if (err != nil) != refused {
				t.Fatalf("refused=%t: %v", refused, err)
			}
			if refused {
				if fake.writes != 0 {
					t.Fatal("refusal mutated project")
				}
				return
			}
			selected, err := runtime.YardAdoptionProjects(context.Background(), fake.Reconcile.Instance)
			proven := scenario == "legacy" || scenario == "current" || scenario == "stale-record"
			if err != nil || (len(selected) == 1) != proven || !proven && len(selected) != 0 {
				t.Fatalf("selected=%v proven=%t: %v", selected, proven, err)
			}
			if scenario == "stale-record" {
				record.ImportedAt = "changed-after-assessment"
				payload, _ = json.Marshal(record)
				testkit.WriteFile(t, path, payload, 0600)
				if err := execution.legacyContinuation.applyAdoption(context.Background(), program, execution); !errors.Is(err, domain.ErrPlanStale) || fake.writes != 0 {
					t.Fatalf("registry change not rejected: %v", err)
				}
				return
			}
			if err := execution.legacyContinuation.applyAdoption(context.Background(), program, execution); err != nil {
				t.Fatal(err)
			}
			if operationStateDigest(fake.Reconcile.Instance) != before || fake.writes != 1 {
				t.Fatal("bind adoption changed workload")
			}
		})
	}
}

// Old yards can hold devices from removed handlers, operators or older recipes.
// The migration cannot be rolled back, so they are retained exactly as found.
func TestYardAdoptionTakesExistingDevicesAsFound(t *testing.T) {
	proxy := func(listen, connect string) map[string]string {
		return map[string]string{"type": "proxy", "bind": "host", "listen": listen, "connect": connect}
	}
	for name, change := range map[string]func(*reconcileruntime.Runtime, *ports.InstanceInfo, *adoptionIncus){
		"historical-proxies": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalDevices["adb-emu"] = proxy("tcp:127.0.0.1:15555", "tcp:127.0.0.1:5555")
			instance.LocalDevices["adb-emu-console"] = proxy("tcp:127.0.0.1:15554", "tcp:127.0.0.1:5554")
		},
		"public-proxy": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalDevices["operator-web"] = proxy("tcp:0.0.0.0:8080", "tcp:127.0.0.1:80")
		},
		"unix-char": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalDevices["operator-char"] = map[string]string{"type": "unix-char", "source": "/dev/ttyUSB0", "path": "/dev/ttyUSB0", "mode": "0666"}
		},
		"noncanonical-kvm": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalDevices["kvm"] = map[string]string{"type": "unix-char", "source": "/dev/kvm", "path": "/dev/kvm", "gid": "0", "mode": "0666"}
		},
		"noncanonical-apparmor": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalDevices["subyard-docker-apparmor"] = map[string]string{"type": "disk", "source": "/etc/fixture", "path": "/sys/module/apparmor/parameters/enabled"}
		},
		"noncanonical-preview": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalDevices[previewroute.DeviceName] = proxy("tcp:127.0.0.1:18080", "tcp:127.0.0.1:80")
		},
		"local-root-override": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalDevices["root"] = map[string]string{"type": "disk", "path": "/", "pool": "default", "size": "99GiB"}
		},
		"local-root-override-with-sized-profile": func(runtime *reconcileruntime.Runtime, instance *ports.InstanceInfo, fake *adoptionIncus) {
			runtime.Environment = append(runtime.Environment, "ROOT_DISK_SIZE=10GiB")
			fake.Reconcile.ProfileDevices["root"]["size"] = "10GiB"
			instance.Devices["root"]["size"] = "10GiB"
			instance.LocalDevices["root"] = map[string]string{"type": "disk", "path": "/", "pool": "default", "size": "99GiB"}
		},
		"managed-device-missing": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			delete(instance.LocalDevices, "ssh")
			delete(instance.Devices, "ssh")
			delete(instance.LocalDevices, "subyard-e2e-routes")
			delete(instance.Devices, "subyard-e2e-routes")
		},
		"leftover-memory-device": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalDevices["subyard-host-memory"] = map[string]string{"type": "disk", "source": "/tmp/meminfo", "path": "/var/lib/subyard/host-meminfo"}
			instance.LocalConfig["user.subyard.host_memory"], instance.Config["user.subyard.host_memory"] = "v2", "v2"
		},
		"unselected-memory-marker-without-device": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalConfig["user.subyard.host_memory"], instance.Config["user.subyard.host_memory"] = "v1", "v1"
		},
		"accepted-device-volatile-state": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalDevices["operator-web"] = proxy("tcp:0.0.0.0:8080", "tcp:127.0.0.1:80")
			instance.LocalConfig["volatile.operator-web.last_state.ready"], instance.Config["volatile.operator-web.last_state.ready"] = "true", "true"
		},
		"stale-preview-marker": func(_ *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			instance.LocalConfig[previewroute.Key], instance.Config[previewroute.Key] = "stale", "stale"
			instance.LocalConfig[ownerpublication.Prefix+previewroute.DeviceName], instance.Config[ownerpublication.Prefix+previewroute.DeviceName] = "stale", "stale"
		},
		"vm-nic-override": func(runtime *reconcileruntime.Runtime, instance *ports.InstanceInfo, _ *adoptionIncus) {
			canonicalAdoptionVMFixture(runtime, instance)
			instance.LocalDevices["eth0"] = map[string]string{"type": "nic", "nictype": "macvlan", "parent": "fixture0"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			instance := &fake.Reconcile.Instance
			change(&runtime, instance, fake)
			for device, values := range instance.LocalDevices {
				instance.Devices[device] = maps.Clone(values)
			}
			before := operationStateDigest(*instance)
			if _, _, _, _, err := runtime.InspectYardAdoption(context.Background()); err != nil {
				t.Fatal("existing device stopped adoption: ", err)
			}
			if _, _, err := runtime.InspectLegacyContinuation(context.Background()); err == nil {
				t.Fatal("non-adopt continuation admitted the same device")
			}
			if operationStateDigest(*instance) != before || fake.writes != 0 || len(fake.ConfigUpdates) != 0 {
				t.Fatal("inspection changed native state")
			}
		})
	}
}

func TestYardAdoptionHistoricalDeviceRefusalRemainsOnlyInContinuation(t *testing.T) {
	_, execution, fake := adoptionFixture(t)
	runtime := execution.platform.(reconcileruntime.Runtime)
	device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:15555", "connect": "tcp:127.0.0.1:5555"}
	fake.Reconcile.Instance.LocalDevices["adb-emu"], fake.Reconcile.Instance.Devices["adb-emu"] = device, maps.Clone(device)
	if _, _, _, _, err := runtime.InspectYardAdoption(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, err := runtime.InspectLegacyContinuation(context.Background())
	if err == nil || err.Error() != `legacy continuation refuses unsupported local device "adb-emu"` {
		t.Fatalf("continuation lost its closed first refusal: %v", err)
	}
}

func TestYardAdoptionRefusesPersistentVolumeIdentityDrift(t *testing.T) {
	for _, scenario := range []string{"other-volume", "other-path", "extra-option", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			_, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			instance := &fake.Reconcile.Instance
			switch scenario {
			case "other-volume":
				instance.LocalDevices["srv"]["source"] = "other-volume"
			case "other-path":
				instance.LocalDevices["srv"]["path"] = "/data"
			case "extra-option":
				instance.LocalDevices["srv"]["readonly"] = "true"
			case "missing":
				delete(instance.LocalDevices, "srv")
				delete(instance.Devices, "srv")
			}
			if device := instance.LocalDevices["srv"]; device != nil {
				instance.Devices["srv"] = maps.Clone(device)
			}
			_, _, _, _, err := runtime.InspectYardAdoption(context.Background())
			if err == nil || !strings.Contains(err.Error(), "yard adoption refuses srv device") {
				t.Fatalf("persistent data identity admitted: %v", err)
			}
			if fake.writes != 0 {
				t.Fatal("refusal mutated project")
			}
		})
	}
}

// A pinned VM and a bounded root leave their profile devices with more than the
// creation recipe. Adoption keeps them; only the identity of each device refuses.
func TestYardAdoptionTakesProfileDevicesAsFound(t *testing.T) {
	const refusal = "yard adoption refuses noncanonical project root or NIC properties"
	for name, test := range map[string]struct {
		device, key, value string
		refused            bool
	}{
		"nic-pinned-address": {device: "eth0", key: "ipv4.address", value: "10.20.30.40"},
		"nic-hwaddr":         {device: "eth0", key: "hwaddr", value: "00:16:3e:00:00:01"},
		"nic-filtering":      {device: "eth0", key: "security.ipv4_filtering", value: "true"},
		"root-size":          {device: "root", key: "size", value: "99GiB"},
		"root-extra":         {device: "root", key: "raw.mount.options", value: "bind"},
		"nic-network":        {device: "eth0", key: "network", value: "foreign", refused: true},
		"nic-type":           {device: "eth0", key: "type", value: "disk", refused: true},
		"root-type":          {device: "root", key: "type", value: "nic", refused: true},
		"root-path":          {device: "root", key: "path", value: "/other", refused: true},
		"root-pool":          {device: "root", key: "pool", value: "foreign", refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			fake.Reconcile.ProfileDevices[test.device][test.key] = test.value
			fake.Reconcile.Instance.Devices[test.device] = maps.Clone(fake.Reconcile.ProfileDevices[test.device])
			_, _, metadata, _, err := runtime.InspectYardAdoption(context.Background())
			if test.refused {
				if err == nil || !strings.Contains(err.Error(), refusal) {
					t.Fatalf("got %v, want %s", err, refusal)
				}
				return
			}
			if err != nil || metadata["user.subyard.installation"] != domain.AdoptedInstallation {
				t.Fatalf("metadata=%v err=%v", metadata, err)
			}
		})
	}
}

// adoptionIngressIncus also publishes a resource receipt the way the native CAS does.
type adoptionIngressIncus struct {
	*adoptionIncus
	instanceWrites int
}

func (fake *adoptionIngressIncus) AdoptRegisteredInstance(_ context.Context, before ports.InstanceInfo, values map[string]string) error {
	current := fake.Reconcile.Instance
	if !maps.Equal(current.LocalConfig, before.LocalConfig) {
		return domain.ErrPlanStale
	}
	for key, value := range values {
		if current.LocalConfig[key] != "" && current.LocalConfig[key] != value {
			return domain.ErrPlanStale
		}
		if current.LocalConfig[key] != value {
			current.LocalConfig[key], current.Config[key] = value, value
			fake.instanceWrites++
		}
	}
	return nil
}

func registeredIngressOwner(t *testing.T) (*migration.RegisteredYardMigration, *adoptionIngressIncus, reconcileruntime.Runtime, map[string]resource.ProxyContract) {
	t.Helper()
	_, execution, base := adoptionFixture(t)
	fake := &adoptionIngressIncus{adoptionIncus: base}
	runtime := execution.platform.(reconcileruntime.Runtime)
	runtime.Incus = fake
	specs := []resource.ResourceSpec{}
	for _, row := range [][2]string{
		{"udp", "sample-udp SAMPLE_HOST SAMPLE_PORT SAMPLE_IF udp:guest:host-port owner-metadata-v1 owner-ipv4-udp\"\nSTARTUP=\"bringup"},
		{"tcp", "sample-tcp SAMPLE_HOST SAMPLE_ADMIN SAMPLE_IF tcp:guest:22 owner-metadata-v1 owner-ipv4-tcp"},
	} {
		spec, err := resource.ParseResource("sample", row[0], strings.NewReader("COMMAND="+row[0]+"\nHANDLER=handler.sh\nTITLE=sample\nPROXY=\""+row[1]+"\"\nACTION=\"up up public-ingress-change reversible\"\nACTION=\"down down public-ingress-change reversible\"\nBRINGUP=up\nSHUTDOWN=down\n"))
		if err != nil {
			t.Fatal(err)
		}
		specs = append(specs, spec)
	}
	resources, err := resource.BuildRegistry(specs)
	if err != nil {
		t.Fatal(err)
	}
	contracts := map[string]resource.ProxyContract{}
	for _, contract := range resources.ProxyContracts([]string{"sample"}) {
		contracts[contract.Device] = contract
	}
	loaded := execution.loaded
	loaded.Context = runtime.Yard
	loaded.Environment = maps.Clone(loaded.Environment)
	loaded.Environment["ENVIRONMENT_PROFILES"] = "sample"
	fake.Instances = map[string]ports.InstanceInfo{runtime.Yard.IncusProject + "/" + runtime.Yard.YardInstanceName: fake.Reconcile.Instance}
	owner := &migration.RegisteredYardMigration{Yard: loaded.Context.YardName, ConfigHome: loaded.Context.Paths.ConfigHome, Resources: resources, LoadTargets: func(context.Context, releasetransition.V2SettingsSnapshotView) ([]migration.RegisteredYardTarget, error) {
		return []migration.RegisteredYardTarget{{Loaded: loaded, Runtime: runtime}}, nil
	}}
	return owner, fake, runtime, contracts
}

func TestRegisteredOwnerIngressEntersManagementAsFound(t *testing.T) {
	const udp, tcp = "sample-udp", "sample-tcp"
	receiptKey := func(device string) string { return "user.subyard.resource." + device }
	startupKey := "user.subyard.startup." + udp
	pending := func(contract resource.ProxyContract, device map[string]string) string {
		return strings.Replace(contract.OwnershipValue(device), "v1:", "v1:pending:", 1)
	}
	type fixture struct {
		instance  *ports.InstanceInfo
		contracts map[string]resource.ProxyContract
	}
	set := func(state fixture, key, value string) {
		state.instance.LocalConfig[key], state.instance.Config[key] = value, value
	}
	remove := func(state fixture, device string) {
		delete(state.instance.LocalDevices, device)
		delete(state.instance.Devices, device)
	}
	receipts := func(state fixture) {
		for device, contract := range state.contracts {
			if local := state.instance.LocalDevices[device]; local != nil {
				set(state, receiptKey(device), contract.OwnershipValue(local))
			}
		}
	}
	for name, test := range map[string]struct {
		mutate  func(fixture)
		publish int
		want    string
	}{
		"receipts-match":  {mutate: receipts},
		"receipts-absent": {mutate: func(fixture) {}, publish: 2},
		"startup-enabled": {mutate: func(state fixture) { receipts(state); set(state, startupKey, "enabled") }},
		"startup-pending": {mutate: func(state fixture) { remove(state, udp); remove(state, tcp); set(state, startupKey, "pending") }},
		"interrupted-bringup": {mutate: func(state fixture) {
			set(state, receiptKey(udp), pending(state.contracts[udp], state.instance.LocalDevices[udp]))
			set(state, receiptKey(tcp), state.contracts[tcp].OwnershipValue(state.instance.LocalDevices[tcp]))
			set(state, startupKey, "enabled")
		}},
		"stopped-with-pending-receipt": {mutate: func(state fixture) {
			set(state, receiptKey(udp), pending(state.contracts[udp], state.instance.LocalDevices[udp]))
			remove(state, udp)
			set(state, startupKey, "disabled")
			set(state, receiptKey(tcp), state.contracts[tcp].OwnershipValue(state.instance.LocalDevices[tcp]))
		}},
		"foreign-receipt":       {mutate: func(state fixture) { set(state, receiptKey(udp), "foreign") }, want: `registered resource "sample-udp": has foreign or inherited receipt`},
		"other-pending-receipt": {mutate: func(state fixture) { set(state, receiptKey(udp), "v1:pending:"+strings.Repeat("0", 64)) }, want: `registered resource "sample-udp": has foreign or inherited receipt`},
		"inherited-receipt": {mutate: func(state fixture) {
			state.instance.Config[receiptKey(udp)] = state.contracts[udp].OwnershipValue(state.instance.LocalDevices[udp])
		}, want: `registered resource "sample-udp": has foreign or inherited receipt`},
		"divergent-device": {mutate: func(state fixture) { state.instance.Devices[udp]["listen"] = "udp:203.0.113.11:51820" }, want: `registered resource "sample-udp": has unsupported or inherited ownership`},
		"not-a-proxy": {mutate: func(state fixture) {
			state.instance.LocalDevices[udp]["type"], state.instance.Devices[udp]["type"] = "disk", "disk"
		}, want: `registered resource "sample-udp": does not match its declared native proxy recipe`},
		"deferred-publication": {mutate: func(state fixture) { set(state, ownerpublication.Prefix+udp, "value") }, want: `registered resource "sample-udp": has unresolved deferred publication`},
		"stopped-with-final-receipt": {mutate: func(state fixture) {
			set(state, receiptKey(udp), state.contracts[udp].OwnershipValue(state.instance.LocalDevices[udp]))
			remove(state, udp)
		}, want: `unsupported instance configuration key "user.subyard.resource.sample-udp"`},
		"stopped-with-inherited-receipt": {mutate: func(state fixture) {
			state.instance.Config[receiptKey(udp)] = pending(state.contracts[udp], state.instance.LocalDevices[udp])
			remove(state, udp)
		}, want: `unsupported instance configuration key "user.subyard.resource.sample-udp"`},
		"startup-unknown-value": {mutate: func(state fixture) { set(state, startupKey, "unknown") }, want: `unsupported instance configuration key "user.subyard.startup.sample-udp"`},
		"startup-inherited":     {mutate: func(state fixture) { state.instance.Config[startupKey] = "enabled" }, want: `unsupported instance configuration key "user.subyard.startup.sample-udp"`},
		"startup-undeclared":    {mutate: func(state fixture) { set(state, "user.subyard.startup."+tcp, "enabled") }, want: `unsupported instance configuration key "user.subyard.startup.sample-tcp"`},
	} {
		t.Run(name, func(t *testing.T) {
			owner, fake, _, contracts := registeredIngressOwner(t)
			instance := &fake.Reconcile.Instance
			for device, listen := range map[string][2]string{udp: {"udp:203.0.113.10:51820", "udp:10.20.30.40:51820"}, tcp: {"tcp:203.0.113.10:2226", "tcp:10.20.30.40:22"}} {
				local := map[string]string{"type": "proxy", "bind": "host", "nat": "true", "listen": listen[0], "connect": listen[1]}
				instance.LocalDevices[device], instance.Devices[device] = local, maps.Clone(local)
			}
			test.mutate(fixture{instance, contracts})
			initial, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("got %v, want %s", err, test.want)
				}
				if fake.writes != 0 || fake.instanceWrites != 0 {
					t.Fatal("refused assessment wrote native state")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			devices, err := json.Marshal([]any{instance.LocalDevices, instance.Devices})
			if err != nil {
				t.Fatal(err)
			}
			before := maps.Clone(instance.LocalConfig)
			if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal(err)
			}
			final, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if err != nil || !final.Converged || final.Desired != initial.Desired || len(final.Consequences) != 0 {
				t.Fatalf("final: %#v %v", final, err)
			}
			after, _ := json.Marshal([]any{instance.LocalDevices, instance.Devices})
			if string(after) != string(devices) || fake.writes != 1 || fake.instanceWrites != test.publish {
				t.Fatalf("native writes=%d instance=%d, want 1 and %d; devices changed=%t", fake.writes, fake.instanceWrites, test.publish, string(after) != string(devices))
			}
			for key, value := range before {
				if instance.LocalConfig[key] != value {
					t.Fatalf("metadata-only adoption rewrote %s", key)
				}
			}
			if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal("repeat", err)
			}
			if fake.instanceWrites != test.publish {
				t.Fatal("repeat wrote another receipt")
			}
		})
	}
}

// Continuation has no owner-ingress concept: these keys stay unsupported on its path.
func TestLegacyContinuationStaysClosedToOwnerIngressMetadata(t *testing.T) {
	for _, key := range []string{"user.subyard.startup.sample-udp", "user.subyard.resource.sample-udp"} {
		t.Run(key, func(t *testing.T) {
			_, fake, runtime, _ := registeredIngressOwner(t)
			instance := &fake.Reconcile.Instance
			value := "enabled"
			if strings.Contains(key, "resource") {
				value = "v1:pending:" + strings.Repeat("0", 64)
			}
			instance.LocalConfig[key], instance.Config[key] = value, value
			_, _, err := runtime.InspectLegacyContinuation(context.Background())
			want := `legacy continuation refuses unsupported instance configuration key "` + key + `"`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want %s", err, want)
			}
		})
	}
}

func registeredRefusalOwner(t *testing.T, execution *initExecution, fake *adoptionIncus) (*migration.RegisteredYardMigration, reconcileruntime.Runtime, resource.ProxyContract) {
	t.Helper()
	runtime := execution.platform.(reconcileruntime.Runtime)
	spec, err := resource.ParseResource("sample", "server", strings.NewReader("COMMAND=server\nHANDLER=handler.sh\nTITLE=sample\nPROXY=\"sample-server SAMPLE_HOST SAMPLE_PORT tcp:127.0.0.1:9119 owner-metadata-v1 loopback-or-tailscale\"\nACTION=\"up up security-change reversible\"\nACTION=\"down down security-change reversible\"\nBRINGUP=up\nSHUTDOWN=down\n"))
	if err != nil {
		t.Fatal(err)
	}
	resources, err := resource.BuildRegistry([]resource.ResourceSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	loaded := execution.loaded
	loaded.Context = runtime.Yard
	loaded.Environment = maps.Clone(loaded.Environment)
	loaded.Environment["ENVIRONMENT_PROFILES"] = "sample"
	loaded.Settings = maps.Clone(loaded.Settings)
	owner := &migration.RegisteredYardMigration{Yard: loaded.Context.YardName, ConfigHome: loaded.Context.Paths.ConfigHome, Resources: resources, LoadTargets: func(context.Context, releasetransition.V2SettingsSnapshotView) ([]migration.RegisteredYardTarget, error) {
		return []migration.RegisteredYardTarget{{Loaded: loaded, Runtime: runtime}}, nil
	}}
	fake.Instances = map[string]ports.InstanceInfo{runtime.Yard.IncusProject + "/" + runtime.Yard.YardInstanceName: fake.Reconcile.Instance}
	return owner, runtime, resources.ProxyContracts([]string{"sample"})[0]
}

// Consequences enter the plan token, so several pending receipts must not make
// an inspected plan stale before convergence.
func TestRegisteredOwnerConsequenceOrderIsStable(t *testing.T) {
	owner, fake, _, _ := registeredIngressOwner(t)
	instance := &fake.Reconcile.Instance
	for device, listen := range map[string][2]string{"sample-udp": {"udp:203.0.113.10:51820", "udp:10.20.30.40:51820"}, "sample-tcp": {"tcp:203.0.113.10:2226", "tcp:10.20.30.40:22"}} {
		local := map[string]string{"type": "proxy", "bind": "host", "nat": "true", "listen": listen[0], "connect": listen[1]}
		instance.LocalDevices[device], instance.Devices[device] = local, maps.Clone(local)
	}
	first, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || len(first.Consequences) < 3 {
		t.Fatalf("initial: %#v %v", first, err)
	}
	for range 32 {
		next, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
		if err != nil || !slices.Equal(next.Consequences, first.Consequences) {
			t.Fatalf("consequence order changed: %q vs %q: %v", next.Consequences, first.Consequences, err)
		}
	}
}

// The labelled migration fixture short-circuits before InspectYardAdoption, so this
// real adoption fixture owns the unlabelled path through the concrete migration.
func TestRegisteredUnlabelledMigrationRetainsUnknownDeviceThroughConvergence(t *testing.T) {
	_, execution, fake := adoptionFixture(t)
	owner, _, _ := registeredRefusalOwner(t, execution, fake)
	instance := &fake.Reconcile.Instance
	device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:15555", "connect": "tcp:127.0.0.1:5555"}
	instance.LocalDevices["adb-emu"], instance.Devices["adb-emu"] = device, maps.Clone(device)
	before, err := json.Marshal(*instance)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || initial.Converged || len(initial.Consequences) != 1 || !strings.Contains(initial.Consequences[0], "all existing local devices enter management as found") {
		t.Fatalf("initial: %#v %v", initial, err)
	}
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
	final, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || !final.Converged || final.Desired != initial.Desired || len(final.Consequences) != 0 || fake.writes != 1 {
		t.Fatalf("final: %#v writes=%d %v", final, fake.writes, err)
	}
	after, err := json.Marshal(*instance)
	if err != nil || string(after) != string(before) {
		t.Fatalf("retained instance changed: %s %v", after, err)
	}
	if fake.Reconcile.ProjectConfig["user.subyard.installation"] != domain.AdoptedInstallation {
		t.Fatal("project ownership was not published")
	}
}

func TestRegisteredMigrationReportsEverySimultaneousRefusal(t *testing.T) {
	_, execution, fake := adoptionFixture(t)
	owner, _, contract := registeredRefusalOwner(t, execution, fake)
	instance := &fake.Reconcile.Instance
	device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:32000", "connect": "tcp:127.0.0.1:9119"}
	instance.LocalDevices[contract.Device], instance.Devices[contract.Device] = device, maps.Clone(device)
	instance.LocalConfig[contract.OwnershipKey()], instance.Config[contract.OwnershipKey()] = "foreign", "foreign"
	instance.LocalDevices["srv"]["source"], instance.Devices["srv"]["source"] = "other-volume", "other-volume"
	instance.LocalConfig["security.privileged"], instance.Config["security.privileged"] = "true", "true"
	fake.Reconcile.ProjectConfig["features.networks"] = "true"
	fake.Reconcile.ProjectConfig["restricted.fixture"] = "allow"
	fake.TeardownResources[2].SnapshotCount = 1
	fake.TeardownResources = append(fake.TeardownResources, ports.TeardownResource{Kind: "bucket", Name: "unproven", Pool: "default", Binding: "bucket"})
	want := []string{
		`yard default: legacy continuation refuses unknown or incomplete native inventory: "bucket/default/unproven"`,
		`legacy continuation refuses unknown project permissions`,
		`legacy continuation refuses unsupported instance configuration key "security.privileged"`,
		`yard adoption refuses srv device that is missing or differs from persistent volume default/yard-srv: reattaching it would change the data mounted in the yard`,
		`yard adoption refuses an uninventoried project feature namespace`,
		`yard adoption refuses unproven snapshots`,
		`registered resource "sample-server": has foreign or inherited receipt`,
	}
	before := operationStateDigest(*instance)
	first := ""
	for run := range 20 {
		_, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
		if err == nil {
			t.Fatal("refusals admitted")
		}
		if run == 0 {
			first = err.Error()
		}
		if err.Error() != first || err.Error() != strings.Join(want, "; ") {
			t.Fatalf("run %d\n got: %s\nwant: %s", run, err, strings.Join(want, "; "))
		}
	}
	if operationStateDigest(*instance) != before || fake.writes != 0 {
		t.Fatal("assessment mutated native state")
	}
}

func TestAdoptionRefusalsSurviveOperatorStderr(t *testing.T) {
	_, execution, fake := adoptionFixture(t)
	runtime := execution.platform.(reconcileruntime.Runtime)
	instance := &fake.Reconcile.Instance
	delete(instance.LocalDevices, "srv")
	delete(instance.Devices, "srv")
	instance.Config["user.subyard.name"], instance.LocalConfig["user.subyard.name"] = "other", "other"
	_, _, _, _, err := runtime.InspectYardAdoption(context.Background())
	if err == nil {
		t.Fatal("refusals admitted")
	}
	for _, local := range []bool{false, true} {
		var stderr bytes.Buffer
		program := &CLI{options: Options{Program: "yard-engine", Stderr: &stderr}, env: map[string]string{}, localMetadataDiagnostics: local}
		program.errorf("release transition: %v", err)
		output := stderr.String()
		if !strings.HasPrefix(output, "yard-engine: release transition: ") || strings.Count(output, "\n") != 1 || !strings.Contains(output, "legacy continuation native identity") || !strings.Contains(output, "yard adoption refuses srv device") {
			t.Fatalf("local=%t lost a refusal: %q", local, output)
		}
		if strings.Contains(output, "user.subyard.name: observed") != local {
			t.Fatalf("local=%t changed owner detail boundary: %q", local, output)
		}
	}
}

func TestYardAdoptionAllowsAbsentSelectedTelemetry(t *testing.T) {
	_, execution, fake := adoptionFixture(t)
	runtime := execution.platform.(reconcileruntime.Runtime)
	runtime.RepositoryRoot = testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(runtime.RepositoryRoot, "config/profiles/telemetry"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "config/profiles/telemetry/profile.conf"), []byte("YARD_CAPS=host-memory\n"), 0600)
	runtime.Environment = append(runtime.Environment, "ENVIRONMENT_PROFILES=telemetry")
	before := operationStateDigest(fake.Reconcile.Instance)
	if _, _, _, _, err := runtime.InspectYardAdoption(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.InspectLegacyContinuation(context.Background()); err == nil {
		t.Fatal("continuation admitted unconverged telemetry")
	}
	if operationStateDigest(fake.Reconcile.Instance) != before || fake.writes != 0 {
		t.Fatal("telemetry adoption mutated workload")
	}
}

func TestYardAdoptionRebuildKeepsNormalInitAuthority(t *testing.T) {
	program, execution, _ := adoptionFixture(t)
	if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{adopt: true}); err != nil {
		t.Fatal(err)
	}
	program.options.InitPlatform = nil
	execution.rebuildPlatform(program)
	runtime := execution.platform.(reconcileruntime.Runtime)
	if runtime.LegacyContinuation != nil {
		t.Fatal("adoption acquired a legacy continuation grant during privilege setup")
	}
}

// Verified candidates run from a parent-pinned /proc/<pid>/fd/N label, so two
// processes render different roots for one unchanged yard.
func TestYardAdoptionContextIsStableAcrossProcessLabels(t *testing.T) {
	_, execution, fake := adoptionFixture(t)
	inspect := func(pid string) map[string]string {
		t.Helper()
		runtime := execution.platform.(reconcileruntime.Runtime)
		runtime.Yard.Paths.RepositoryRoot = "/proc/" + pid + "/fd/7"
		runtime.Yard.Paths.ConfigDir = "/proc/" + pid + "/fd/7/config"
		_, _, metadata, _, err := runtime.InspectYardAdoption(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return metadata
	}
	first, second := inspect("111"), inspect("222")
	if !maps.Equal(first, second) {
		t.Fatalf("new adoption metadata differs across processes:\n%#v\n%#v", first, second)
	}
	if strings.Contains(first["user.subyard.context"], "/proc/") {
		t.Fatalf("process-local path published: %s", first["user.subyard.context"])
	}
	if fake.writes != 0 {
		t.Fatal("inspection wrote project metadata")
	}
}

func TestYardAdoptionAcceptsOnlyProcessLabelsInStampedContext(t *testing.T) {
	stamp := func(t *testing.T, change func(*domain.Context) string) (reconcileruntime.Runtime, *adoptionIncus, string) {
		t.Helper()
		_, execution, fake := adoptionFixture(t)
		runtime := execution.platform.(reconcileruntime.Runtime)
		_, _, metadata, _, err := runtime.InspectYardAdoption(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var stamped domain.Context
		if err := json.Unmarshal([]byte(metadata["user.subyard.context"]), &stamped); err != nil {
			t.Fatal(err)
		}
		stamped.Paths.RepositoryRoot = "/proc/183242/fd/7"
		stamped.Paths.ConfigDir = "/proc/183242/fd/7/config"
		payload, err := json.Marshal(stamped)
		if err != nil {
			t.Fatal(err)
		}
		value := string(payload)
		if change != nil {
			value = change(&stamped)
		}
		maps.Copy(fake.Reconcile.ProjectConfig, metadata)
		fake.Reconcile.ProjectConfig["user.subyard.context"] = value
		return runtime, fake, value
	}
	t.Run("labels as found", func(t *testing.T) {
		runtime, fake, value := stamp(t, nil)
		for _, pid := range []string{"111", "222"} {
			runtime.Yard.Paths.RepositoryRoot = "/proc/" + pid + "/fd/9"
			_, _, metadata, before, err := runtime.InspectYardAdoption(context.Background())
			if err != nil {
				t.Fatal("stamped label refused: ", err)
			}
			if metadata["user.subyard.context"] != value || before["user.subyard.context"] != value {
				t.Fatalf("stamped context was not kept as found: %s", metadata["user.subyard.context"])
			}
		}
		if fake.writes != 0 {
			t.Fatal("accepting a label rewrote project metadata")
		}
	})
	marshal := func(t *testing.T, context domain.Context) string {
		t.Helper()
		payload, err := json.Marshal(context)
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	for name, change := range map[string]func(*testing.T, *domain.Context) string{
		"project": func(t *testing.T, value *domain.Context) string {
			value.IncusProject = "foreign"
			return marshal(t, *value)
		},
		"yard": func(t *testing.T, value *domain.Context) string {
			value.YardName = "foreign"
			return marshal(t, *value)
		},
		"config home": func(t *testing.T, value *domain.Context) string {
			value.Paths.ConfigHome = "/foreign"
			return marshal(t, *value)
		},
		"unknown field": func(t *testing.T, value *domain.Context) string {
			return strings.TrimSuffix(marshal(t, *value), "}") + `,"foreign":true}`
		},
		"trailing data": func(t *testing.T, value *domain.Context) string { return marshal(t, *value) + "{}" },
		"not canonical": func(t *testing.T, value *domain.Context) string { return " " + marshal(t, *value) },
		"empty":         func(*testing.T, *domain.Context) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			runtime, _, _ := stamp(t, func(value *domain.Context) string { return change(t, value) })
			_, _, _, _, err := runtime.InspectYardAdoption(context.Background())
			for _, want := range []string{"partial, foreign or conflicting project ownership", "incomplete adopted project ownership"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q missing: %v", want, err)
				}
			}
		})
	}
}

// The ownership binding a journal records must hold in every later process, for a
// new adoption and for a project stamped by an earlier process as found.
func TestRegisteredAdoptionBindingIsStableAcrossProcessLabels(t *testing.T) {
	_, execution, fake := adoptionFixture(t)
	reference, base, _ := registeredRefusalOwner(t, execution, fake)
	load := reference.LoadTargets
	observe := func(pid string) (releasetransition.Fingerprint, map[string]string) {
		t.Helper()
		runtime := base
		runtime.Yard.Paths.RepositoryRoot = "/proc/" + pid + "/fd/7"
		runtime.Yard.Paths.ConfigDir = "/proc/" + pid + "/fd/7/config"
		owner := *reference
		owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]migration.RegisteredYardTarget, error) {
			targets, err := load(ctx, view)
			for index := range targets {
				targets[index].Runtime = runtime
			}
			return targets, err
		}
		observed, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
		if err != nil {
			t.Fatal(err)
		}
		_, _, metadata, _, err := runtime.InspectYardAdoption(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return observed.Desired, metadata
	}
	unstamped, metadata := observe("111")
	if again, _ := observe("222"); again != unstamped {
		t.Fatal("new adoption Desired differs across processes")
	}
	maps.Copy(fake.Reconcile.ProjectConfig, metadata)
	var stamped domain.Context
	if err := json.Unmarshal([]byte(metadata["user.subyard.context"]), &stamped); err != nil {
		t.Fatal(err)
	}
	stamped.Paths.RepositoryRoot = "/proc/183242/fd/7"
	payload, err := json.Marshal(stamped)
	if err != nil {
		t.Fatal(err)
	}
	fake.Reconcile.ProjectConfig["user.subyard.context"] = string(payload)
	first, _ := observe("333")
	if second, _ := observe("444"); second != first {
		t.Fatal("stamped adoption Desired differs across processes")
	}
	if fake.writes != 0 {
		t.Fatal("observation wrote project metadata")
	}
}
