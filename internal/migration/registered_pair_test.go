package migration

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func pairedRegisteredOwners(t *testing.T) (*RegisteredYardMigration, *RegisteredYardMigration, *registeredNativeFixture, string) {
	t.Helper()
	owner, native, path := registeredMigrationFixture(t)
	physicalRoot := testkit.TempDir(t)
	policy, err := os.ReadFile("../../scripts/guest-disk-tmp.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(physicalRoot, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"guest-disk-tmp.sh": policy, "06-network.sh": []byte("#!/bin/sh\nexit 0\n"), "lifecycle-guard.sh": []byte("#!/bin/sh\nexit 0\n")} {
		testkit.WriteFile(t, filepath.Join(physicalRoot, "scripts", name), body, 0700)
	}
	original := owner.LoadTargets
	owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
		targets, err := original(ctx, view)
		if err != nil {
			return nil, err
		}
		for index := range targets {
			loaded := targets[index].Loaded
			defaults := maps.Clone(loaded.Environment)
			defaults["SAMPLE_HOST"] = "localhost"

			snapshot, err := config.ReadPersistentFileSnapshot(owner.ConfigHome, path)
			if view != nil {
				snapshot, err = view.ReadSnapshot(path)
			}
			if err != nil {
				return nil, err
			}
			assignments, err := config.ParsePersistentAssignments(path, snapshot.Content)
			if err != nil {
				return nil, err
			}
			values, authored := defaults, map[string]string{}
			for _, assignment := range assignments {
				values[assignment.Name] = assignment.Value
				authored[assignment.Name] = assignment.Value
			}

			loaded.Environment = values
			loaded.Settings = maps.Clone(loaded.Settings)
			for _, key := range []string{"SAMPLE_HOST", "SAMPLE_PORT", "ENVIRONMENT_PROFILES"} {
				trace := loaded.Settings[key]
				trace.EffectiveValue = values[key]
				trace.Resolutions = []config.SettingResolution{{Scope: "default", Role: "shipped defaults", Status: "effective"}}
				if _, ok := authored[key]; ok {
					trace.Resolutions = []config.SettingResolution{{Scope: "yard", Role: "local settings", Status: "effective"}}
				}
				loaded.Settings[key] = trace
			}
			loaded.ConfigurationLayers = []config.ConfigurationLayer{{Path: path, Present: true}}
			targets[index].Loaded = loaded
			targets[index].Runtime.RepositoryRoot = physicalRoot
			targets[index].Runtime.NetworkPolicy = restartNetwork{}
			targets[index].Runtime.Executor = native
		}
		return targets, nil
	}
	tmp := &RegisteredYardMigration{Yard: owner.Yard, ConfigHome: owner.ConfigHome, Resources: owner.Resources, GuestTmp: true, LoadTargets: owner.LoadTargets}
	return owner, tmp, native, path
}

func TestGuestTmpBindsInheritedNativeStateBeforeMutation(t *testing.T) {
	for _, change := range []string{"config", "device", "profile", "volume"} {
		t.Run(change, func(t *testing.T) {
			_, tmp, native, _ := pairedRegisteredOwners(t)
			if _, err := tmp.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal(err)
			}
			current := native.Instances["fixture/yard"]
			switch change {
			case "config":
				current.Config = maps.Clone(current.Config)
				current.Config["security.privileged"] = "true"
			case "device":
				current.Devices = maps.Clone(current.Devices)
				current.Devices["foreign"] = map[string]string{"type": "disk", "source": "/foreign", "path": "/foreign"}
			case "profile":
				native.Reconcile.ProfileConfig = map[string]string{"security.privileged": "true"}
			case "volume":
				native.TeardownResources = []ports.TeardownResource{{Kind: "volume", Name: "srv", Pool: "pool", Binding: "changed"}}
			}
			native.Instances["fixture/yard"] = current
			native.Reconcile.Instance = current
			err := tmp.Reconcile(context.Background(), releasetransition.ReleaseLinks{})
			if err == nil || native.grants != 0 {
				t.Fatalf("inherited drift received mutation: err=%v grants=%d", err, native.grants)
			}
			for _, call := range native.ExecCalls {
				if call.Request.Command[len(call.Request.Command)-1] == "apply" {
					t.Fatal("guest policy applied after stale native observation")
				}
			}
		})
	}
}

func TestRegisteredEndpointSettingsRetainActiveFlatRegistration(t *testing.T) {
	home := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(home, "yards"), 0700); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(home, "yards/named.env")
	testkit.WriteFile(t, flat, []byte("YARD_KIND=container\nINCUS_PROJECT=existing\n"), 0600)
	path, err := config.LocalYardSettingsFile(filepath.Join(home, "yards"), "named")
	if err != nil || path != flat {
		t.Fatalf("migration would shadow active flat registration: %s %v", path, err)
	}
	before, err := config.ReadPersistentFileSnapshot(home, path)
	if err != nil {
		t.Fatal(err)
	}
	value := "127.0.0.1"
	desired, err := config.EditPersistentAssignmentContent(path, before.Content, "SAMPLE_HOST", &value)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.CompareAndSwapLocalSettingsFile(home, path, before, desired); err != nil {
		t.Fatal(err)
	}
	registration, err := config.FindYardRegistrationFile(home, "named")
	values, readErr := config.ReadAssignments(registration)
	if err != nil || readErr != nil || registration != flat || values["YARD_KIND"] != "container" || values["INCUS_PROJECT"] != "existing" || values["SAMPLE_HOST"] != value {
		t.Fatalf("flat identity was shadowed or changed: %s %#v %v %v", registration, values, err, readErr)
	}
	if _, err := os.Lstat(filepath.Join(home, "yards/named/config.env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new shadow registration was created")
	}

}

func TestGuestTmpMarkerDoesNotRewriteDesiredPower(t *testing.T) {
	_, tmp, native, _ := pairedRegisteredOwners(t)
	instance := native.Instances["fixture/yard"]
	for _, values := range []map[string]string{instance.Config, instance.LocalConfig} {
		values["user.subyard.desired_power"] = "stopped"
		values["user.subyard.disk_tmp"] = "v1"
	}
	native.Instances["fixture/yard"] = instance
	native.Reconcile.Instance = instance
	observed, err := tmp.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
	if err != nil || !observed.Converged {
		t.Fatalf("completed marker reopened power intent: %#v %v", observed, err)
	}
}

// Reuse shipped configuration through a fixture-owned root; only the synthetic
// profile declaration is new. Git/settings trees stay outside the workspace.
func registeredSSHConfigFixture(t *testing.T) string {
	t.Helper()
	root := testkit.TempDir(t)
	source, err := filepath.Abs("../../config")
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "config")
	if err := os.MkdirAll(filepath.Join(destination, "profiles"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0700)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		testkit.WriteFile(t, destination, content, info.Mode().Perm())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sample := filepath.Join(destination, "profiles/sample")
	if err := os.Mkdir(sample, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(sample, "profile.json"), []byte(`{"schema_version":1,"settings":[{"name":"SAMPLE_HOST","type":"string","scopes":["yard","shipped"],"application":"next-command","syncable":true},{"name":"SAMPLE_PORT","type":"port","minimum":1,"maximum":65535,"scopes":["yard","shipped"],"application":"next-command","syncable":true}]}`), 0600)
	testkit.WriteFile(t, filepath.Join(sample, "profile.conf"), []byte("SAMPLE_HOST=localhost\nSAMPLE_PORT=31000\n"), 0600)
	return destination
}

func TestRegisteredBindingRetainsVerifiedAssetAliases(t *testing.T) {
	base, native, _ := registeredMigrationFixture(t)
	root := filepath.Dir(registeredSSHConfigFixture(t))
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile("../../scripts/guest-disk-tmp.sh")
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts/guest-disk-tmp.sh"), policy, 0600)
	testkit.WriteFile(t, filepath.Join(root, "scripts/06-network.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	testkit.WriteFile(t, filepath.Join(root, "scripts/lifecycle-guard.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	first, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	aliases := []string{fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), first.Fd()), fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), second.Fd())}
	makeOwners := func(alias string) (*RegisteredYardMigration, *RegisteredYardMigration, *testkit.Prompt) {
		prompt := &testkit.Prompt{Answers: []bool{false}}
		load := func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
			var files *config.ProspectiveFiles
			if view != nil {
				names, err := view.ListYards()
				if err != nil {
					return nil, err
				}
				files = &config.ProspectiveFiles{Root: base.ConfigHome, Yards: names, ReadSnapshot: view.ReadSnapshot}
			}
			loaded, err := config.Load(config.LoadOptions{RepositoryRoot: alias, OperatorHome: base.ConfigHome, YardName: "default", ProspectiveFiles: files, Environment: map[string]string{"HOME": base.ConfigHome, "SUBYARD_OPERATOR_HOME": base.ConfigHome, "SUBYARD_CONFIG_HOME": base.ConfigHome, "SUBYARD_HOME": base.ConfigHome + "/data", "INCUS_PROJECT": "fixture", "INCUS_BRIDGE": "fixture-br", "YARD_INSTANCE_NAME": "yard", "CODING_TOOL_INTEGRATIONS": "codex"}})
			if err != nil {
				return nil, err
			}
			runtime := reconcileruntime.Runtime{RepositoryRoot: alias, Incus: native, Executor: native, NetworkPolicy: restartNetwork{}, Yard: loaded.Context}
			return []RegisteredYardTarget{{Loaded: loaded, Runtime: runtime}}, nil
		}
		owner := &RegisteredYardMigration{Yard: "default", ConfigHome: base.ConfigHome, Resources: base.Resources, LoadTargets: load}
		tmp := &RegisteredYardMigration{Yard: "default", ConfigHome: base.ConfigHome, Resources: base.Resources, LoadTargets: load, GuestTmp: true}
		return owner, tmp, prompt
	}
	_, tmp, _ := makeOwners(aliases[1])
	// Only immutable asset labels may disappear from the descriptive binding.
	plans, err := tmp.plans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	originalPlan := plans[0]
	originalBinding, err := tmp.binding(originalPlan)
	if err != nil {
		t.Fatal(err)
	}
	fileSetting := ""
	for key, trace := range originalPlan.loaded.Settings {
		if registeredMigrationDefault(trace) && (trace.Kind == config.SettingFile || trace.Type == config.SettingRegularFilePath) && strings.HasPrefix(trace.EffectiveValue, aliases[1]+"/") {
			fileSetting = key
			break
		}
	}
	if fileSetting == "" {
		t.Fatal("missing real shipped asset setting")
	}
	for _, change := range []string{"config-home", "data-home", "external-config", "relative-asset"} {
		changed := originalPlan
		changed.loaded.Settings = maps.Clone(originalPlan.loaded.Settings)
		switch change {
		case "config-home":
			changed.loaded.Context.Paths.ConfigHome += "-different"
		case "data-home":
			changed.loaded.Context.Paths.DataHome += "-different"
		case "external-config":
			changed.loaded.Context.Paths.ConfigDir = base.ConfigHome + "/external-config"
		case "relative-asset":
			trace := changed.loaded.Settings[fileSetting]
			trace.EffectiveValue += ".different"
			changed.loaded.Settings[fileSetting] = trace
		}
		binding, err := tmp.binding(changed)
		if err != nil || migrationFingerprint(binding) == migrationFingerprint(originalBinding) {
			t.Fatalf("%s was erased from binding: %v", change, err)
		}
	}
	// Explicit operator source paths remain exact even when both spellings
	// happen to address the same live verified directory.
	_, firstOwner, _ := makeOwners(aliases[0])
	firstPlans, err := firstOwner.plans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstDefault, err := firstOwner.binding(firstPlans[0])
	if err != nil || migrationFingerprint(firstDefault) != migrationFingerprint(originalBinding) {
		t.Fatal("fresh default asset roles differ before explicit source change", err)
	}
	for _, plan := range []*registeredYardPlan{&firstPlans[0], &originalPlan} {
		plan.loaded.Settings = maps.Clone(plan.loaded.Settings)
		trace := plan.loaded.Settings[fileSetting]
		trace.Resolutions = []config.SettingResolution{{Scope: "yard", Role: "local settings", Status: "effective"}}
		plan.loaded.Settings[fileSetting] = trace
	}
	firstExplicit, err := firstOwner.binding(firstPlans[0])
	if err != nil {
		t.Fatal(err)
	}
	secondExplicit, err := tmp.binding(originalPlan)
	if err != nil {
		t.Fatal(err)
	}
	if migrationFingerprint(firstExplicit) == migrationFingerprint(secondExplicit) {
		t.Fatal("explicit operator file values collapsed across aliases")
	}

}

func TestRegisteredDesiredIgnoresVMRestartOutputs(t *testing.T) {
	// The plan is taken with the project grant absent or explicitly blocked.
	for _, variant := range []struct {
		guest   bool
		planned string
	}{{false, "absent"}, {false, "block"}, {true, "absent"}, {true, "block"}} {
		t.Run(fmt.Sprintf("guest-%t/planned-%s", variant.guest, variant.planned), func(t *testing.T) {
			owner, tmp, native, _ := pairedRegisteredOwners(t)
			if variant.guest {
				owner = tmp
			}
			original := owner.LoadTargets
			owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
				targets, err := original(ctx, view)
				for index := range targets {
					targets[index].Loaded.Context.YardKind = domain.YardVM
					targets[index].Runtime.Yard.YardKind = domain.YardVM
				}
				return targets, err
			}
			instance := native.Instances["fixture/yard"]
			instance.Type = domain.YardVM
			set := func(key, value string) {
				instance.Config[key], instance.LocalConfig[key] = value, value
				native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
			}
			desired := func() releasetransition.Fingerprint {
				t.Helper()
				fresh := *owner
				observed, err := fresh.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
				if err != nil {
					t.Fatal(err)
				}
				return observed.Desired
			}
			set("volatile.eth0.host_name", "tapBEFORE")
			set("volatile.vm.rtc_adjustment", "-1")
			if variant.planned == "block" {
				native.Reconcile.ProjectConfig["restricted.virtual-machines.lowlevel"] = "block"
			}
			before := desired()
			set("volatile.eth0.host_name", "tapAFTER")
			set("volatile.vm.rtc_adjustment", "-2")
			set("volatile.vm.needs_reset", "true")
			// The product start policy writes the owner device override and replaces an
			// absent or blocked project grant with "allow".
			set("raw.qemu.conf", domain.VMRawQEMUConfig(false))
			native.Reconcile.ProjectConfig["restricted.virtual-machines.lowlevel"] = "allow"
			if desired() != before {
				t.Fatal("VM restart outputs changed the pinned Desired")
			}
		})
	}
}

func TestRegisteredMigrationBindsNonClockVMDrift(t *testing.T) {
	for _, guest := range []bool{false, true} {
		for _, change := range []string{"malformed-clock", "noncanonical-clock", "clock-suffix", "container-clock", "reset-false", "reset-empty", "reset-suffix", "container-reset", "uuid", "generation", "vsock", "limits", "device", "project-policy", "container-lowlevel-block", "container-lowlevel"} {
			t.Run(fmt.Sprintf("guest-%t/%s", guest, change), func(t *testing.T) {
				container := strings.HasPrefix(change, "container-")
				owner, tmp, native, _ := pairedRegisteredOwners(t)
				if guest {
					owner = tmp
				}
				original := owner.LoadTargets
				owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
					targets, err := original(ctx, view)
					if !container {
						for index := range targets {
							targets[index].Loaded.Context.YardKind = domain.YardVM
							targets[index].Runtime.Yard.YardKind = domain.YardVM
						}
					}
					return targets, err
				}
				instance := native.Instances["fixture/yard"]
				if !container {
					instance.Type = domain.YardVM
				}
				for _, values := range []map[string]string{instance.Config, instance.LocalConfig} {
					values["volatile.vm.rtc_adjustment"], values["volatile.vm.rtc_offset"] = "0", "0"
				}
				native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
				before, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
				if err != nil {
					t.Fatal(err)
				}
				key, value := "volatile.vm.rtc_offset", "1"
				switch change {
				case "malformed-clock":
					value = "invalid"
				case "noncanonical-clock":
					value = "01"
				case "clock-suffix":
					key = "volatile.vm.rtc_offset.extra"
				case "reset-false":
					key, value = "volatile.vm.needs_reset", "false"
				case "reset-empty":
					key, value = "volatile.vm.needs_reset", ""
				case "reset-suffix":
					key, value = "volatile.vm.needs_reset.extra", "true"
				case "container-reset":
					key, value = "volatile.vm.needs_reset", "true"
				case "uuid":
					key = "volatile.uuid"
				case "generation":
					key = "volatile.uuid.generation"
				case "vsock":
					key = "volatile.vsock_id"
				case "container-lowlevel-block", "container-lowlevel":
					value = map[string]string{"container-lowlevel-block": "block", "container-lowlevel": "allow"}[change]
					native.Reconcile.ProjectConfig["restricted.virtual-machines.lowlevel"] = value
				case "project-policy":
					// Another policy key of a VM yard's project stays bound.
					native.Reconcile.ProjectConfig["restricted.devices.disk"] = "allow"
				case "limits":
					key = "limits.cpu"
				case "device":
					instance.Devices = maps.Clone(instance.Devices)
					instance.Devices["foreign"] = map[string]string{"type": "disk", "source": "/foreign", "path": "/foreign"}
				}
				if change != "device" && change != "project-policy" && !strings.Contains(change, "lowlevel") {
					for _, values := range []map[string]string{instance.Config, instance.LocalConfig} {
						values[key] = value
					}
				}
				native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
				fresh := *owner
				after, err := fresh.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
				if err == nil && after.Desired == before.Desired {
					t.Fatal("unapproved native drift retained migration authority")
				}
			})
		}
	}
}

func TestGuestTmpCompletionDoesNotRequireOrdinaryInitialization(t *testing.T) {
	for _, initialized := range []string{"", "false", "true"} {
		t.Run("initialized-"+initialized, func(t *testing.T) {
			_, owner, native, _ := pairedRegisteredOwners(t)
			instance := native.Instances["fixture/yard"]
			instance.Status = "Stopped"
			for _, values := range []map[string]string{instance.Config, instance.LocalConfig} {
				values["user.subyard.desired_power"], values["user.subyard.disk_tmp"] = "stopped", "v1"
				delete(values, "user.subyard.initialized")
				if initialized != "" {
					values["user.subyard.initialized"] = initialized
				}
			}
			native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
			before, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal(err)
			}
			after, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if err != nil || !after.Converged || after.Desired != before.Desired {
				t.Fatalf("completed policy remains pending after no-op apply: before=%t after=%t err=%v", before.Converged, after.Converged, err)
			}
			actual := native.Instances["fixture/yard"]
			if actual.Config["user.subyard.initialized"] != initialized || actual.Status != "Stopped" || native.grants != 0 || len(native.ExecCalls) != 0 {
				t.Fatal("policy migration initialized or touched a stopped yard")
			}
			if application.InstanceIntentionallyStopped(actual) != (initialized == "true") {
				t.Fatal("ordinary initialization readiness was relaxed")
			}
		})
	}
	for _, invalid := range []string{"managed", "desired", "marker", "inherited", "uuid"} {
		t.Run("invalid-"+invalid, func(t *testing.T) {
			_, owner, native, _ := pairedRegisteredOwners(t)
			instance := native.Instances["fixture/yard"]
			instance.Status = "Stopped"
			for _, values := range []map[string]string{instance.Config, instance.LocalConfig} {
				values["user.subyard.desired_power"], values["user.subyard.disk_tmp"] = "stopped", "v1"
				delete(values, "user.subyard.initialized")
				key, value := map[string]string{"managed": "user.subyard.managed", "desired": "user.subyard.desired_power", "marker": "user.subyard.disk_tmp", "uuid": "volatile.uuid"}[invalid], "foreign"
				if invalid == "uuid" {
					value = ""
				}
				if key != "" {
					values[key] = value
				}
			}
			if invalid == "inherited" {
				delete(instance.LocalConfig, "user.subyard.disk_tmp")
			}
			native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
			if _, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{}); err == nil {
				t.Fatal("invalid ownership, policy or intent admitted")
			}
			if native.grants != 0 || len(native.ExecCalls) != 0 {
				t.Fatal("invalid inspection caused effects")
			}
		})
	}
}
