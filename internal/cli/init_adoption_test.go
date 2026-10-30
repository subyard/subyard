package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
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
			if err != nil || !delta.Changed || len(execution.operationSteps()) != 1 || fake.writes != 0 {
				t.Fatal("adoption not assessed read-only", err)
			}
			if err := execution.run(context.Background(), program, io.Discard); err != nil {
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
	for _, scenario := range []string{"drift", "retirement-interrupted", "retirement-replaced", "foreign-metadata", "snapshot", "bucket", "root-extra", "feature-namespace", "shared-profile", "shared-volume", "bucket-alias", "image-alias", "invalid-feature"} {
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
			if scenario == "root-extra" {
				fake.Reconcile.ProfileDevices["root"]["raw.mount.options"] = "bind"
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
				if err := execution.run(context.Background(), program, io.Discard); err == nil {
					t.Fatal("retired concurrent record replacement")
				}
				fake.afterWrite = nil
				execution.legacyContinuation = nil
				if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{adopt: true}); err != nil {
					t.Fatal(err)
				}
				if err := execution.run(context.Background(), program, io.Discard); err != nil {
					t.Fatal(err)
				}
				return
			}
			if scenario == "drift" {
				fake.Reconcile.Instance.Config["volatile.uuid"] = "replacement"
				if err := execution.run(context.Background(), program, io.Discard); !errors.Is(err, domain.ErrPlanStale) || fake.writes != 0 {
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
			if err := execution.run(context.Background(), program, io.Discard); err != nil {
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
	if err := execution.run(context.Background(), program, io.Discard); !errors.Is(err, domain.ErrPlanStale) || fake.writes != 0 {
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
			if err := execution.run(context.Background(), program, io.Discard); err != nil {
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

func TestYardAdoptionPreparedCommandRequiresConsent(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(map[bool]string{false: "decline", true: "accept"}[accept], func(t *testing.T) {
			program, execution, fake := adoptionFixture(t)
			program.options.InitPlatform = execution.platform
			var output bytes.Buffer
			program.options.Stdout = &output
			program.options.Stderr = &output
			definition, _ := program.manifest.Lookup("init")
			prepared := &preparedCommand{lifecycle: &preparedLifecycle{}, CLI: program, Definition: definition, Loaded: execution.loaded, Arguments: []string{"--adopt"}}
			prepared.policy = commandPolicy(definition, execution.loaded.Context, nil, nil)
			if err := prepared.prepareInit(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if err := prepared.preparePlan(context.Background()); err != nil {
				t.Fatal(err)
			}
			result := program.runPreparedCommand(context.Background(), prepared, accept)
			if accept {
				if result != 0 || fake.writes != 1 {
					t.Fatal("explicit adoption failed", result, output.String())
				}
			} else if result == 0 || fake.writes != 0 {
				t.Fatal("declined adoption wrote ownership", result, output.String())
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
	for _, scenario := range []string{"9p", "virtiofs", "stopped", "unknown-device", "undeclared-disk", "wrong-type", "unknown-suffix", "invalid-bus", "unresolved-bus", "container", "legacy", "disk-policy-drift"} {
		t.Run(scenario, func(t *testing.T) {
			_, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			runtime.Environment = append(runtime.Environment, "HOST_MOUNTS=fixture-mount:/mnt/fixture:ro:0755")
			canonicalLegacyInstanceFixture(runtime, fake.Incus)
			instance := &fake.Reconcile.Instance
			if scenario != "container" && scenario != "legacy" {
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
			case "unknown-suffix":
				key += ".extra"
			case "invalid-bus":
				value = "unrecognized"
			case "unresolved-bus":
				value = "auto"
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
			if scenario != "9p" && scenario != "virtiofs" && scenario != "stopped" {
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
			allow := scenario == "vm-relay" || scenario == "container-proxy" || scenario == "legacy-proxy"
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
			success := scenario == "legacy" || scenario == "current" || scenario == "stale-record"
			if (err == nil) != success {
				t.Fatalf("eligible=%t: %v", success, err)
			}
			if !success {
				if fake.writes != 0 {
					t.Fatal("refusal mutated project")
				}
				return
			}
			if scenario == "stale-record" {
				record.ImportedAt = "changed-after-assessment"
				payload, _ = json.Marshal(record)
				testkit.WriteFile(t, path, payload, 0600)
				if err := execution.run(context.Background(), program, io.Discard); !errors.Is(err, domain.ErrPlanStale) || fake.writes != 0 {
					t.Fatalf("registry change not rejected: %v", err)
				}
				return
			}
			if err := execution.run(context.Background(), program, io.Discard); err != nil {
				t.Fatal(err)
			}
			if operationStateDigest(fake.Reconcile.Instance) != before || fake.writes != 1 {
				t.Fatal("bind adoption changed workload")
			}
		})
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
