package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type stoppedActivationNative struct {
	*adoptionIncus
	statusPath  string
	markerPower string
}

func (native *stoppedActivationNative) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	instance, err := native.Incus.Instance(ctx, project, name)
	if err != nil {
		return instance, err
	}
	body, _ := json.Marshal(instance)
	instance = ports.InstanceInfo{}
	_ = json.Unmarshal(body, &instance)
	status, err := os.ReadFile(native.statusPath)
	instance.Status = string(status)
	return instance, err
}
func (native *stoppedActivationNative) ReconcileState(ctx context.Context, project, name, _, _, _ string) (ports.ReconcileState, error) {
	state := native.Reconcile
	var err error
	state.Instance, err = native.Instance(ctx, project, name)
	return state, err
}
func (native *stoppedActivationNative) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) == 4 && request.Command[0] == "sh" && request.Command[1] == "-s" {
		return ports.InstanceExecResult{}, nil
	}
	return (adoptionHookExecutor{}).Exec(ctx, project, name, request)
}
func (native *stoppedActivationNative) AdoptRegisteredInstance(ctx context.Context, before ports.InstanceInfo, values map[string]string) error {
	actual, err := native.Instance(ctx, before.Project, before.Name)
	if err != nil {
		return err
	}
	if values["user.subyard.disk_tmp"] != "" {
		native.markerPower = actual.Status
	}
	for key, value := range values {
		actual.Config[key], actual.LocalConfig[key] = value, value
	}
	native.Instances[before.Project+"/"+before.Name] = actual
	return nil
}

func TestStoppedGuestMigrationRetainsPreparedUnavailableIntegrationGuard(t *testing.T) {
	for _, mode := range []string{"tmp-stopped", "native-stopped", "combined-stopped", "retired-stopped", "retired-proven"} {
		t.Run(mode, func(t *testing.T) {
			initial := "stopped"
			nativePending := mode == "native-stopped" || mode == "combined-stopped"
			program, execution, fixture := adoptionFixture(t)
			root := program.options.RepositoryRoot
			for _, path := range []string{"scripts/lib/project-config.sh", "scripts/guest-disk-tmp.sh", "config/projects-changed.sh"} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(root, path), mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), path)), 0600)
			}
			status := filepath.Join(root, "physical-status")
			testkit.WriteFile(t, status, []byte(initial), 0600)
			instance := fixture.Reconcile.Instance
			instance.Config["user.subyard.desired_power"], instance.LocalConfig["user.subyard.desired_power"] = "stopped", "stopped"
			if mode == "native-stopped" {
				instance.Config["user.subyard.disk_tmp"], instance.LocalConfig["user.subyard.disk_tmp"] = "v1", "v1"
			}
			if nativePending {
				device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:32000", "connect": "tcp:127.0.0.1:9119"}
				instance.LocalDevices["sample-server"], instance.Devices["sample-server"] = device, device
				resourceDir := filepath.Join(root, "config/profiles/sample/resources/proxy")
				if err := os.MkdirAll(resourceDir, 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, resourceDir+".res", []byte("COMMAND=sample-server\nHANDLER=resources/proxy/handler.sh\nTITLE=sample\nPROXY=\"sample-server SAMPLE_HOST SAMPLE_PORT tcp:127.0.0.1:9119 owner-metadata-v1 loopback-or-tailscale\"\nACTION=\"up up security-change reversible\"\nACTION=\"down down security-change reversible\"\nBRINGUP=up\nSHUTDOWN=down\n"), 0600)
				testkit.WriteFile(t, filepath.Join(resourceDir, "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			}
			native := &stoppedActivationNative{adoptionIncus: fixture, statusPath: status}
			native.Instances = map[string]ports.InstanceInfo{instance.Project + "/" + instance.Name: instance}
			native.Reconcile.Instance = instance
			options := program.options
			options.InitPlatform, options.Incus, options.Executor = nil, native, native
			policy, _ := yardnetwork.Decode(nil)
			options.NetworkPolicy = &yardnetwork.Service{Host: &networkCLIHost{policy: policy, ready: true, instances: map[string]ports.InstanceInfo{"default": instance}}, Lock: testNetworkPolicyLock{}}
			options.Environment = append(options.Environment, "STATE="+root)
			options.Environment = append(options.Environment, "SAMPLE_HOST=127.0.0.1", "SAMPLE_PORT=32000")
			retired := strings.HasPrefix(mode, "retired-")
			if retired {
				packageRoot := filepath.Join(root, "config/retired-integrations/widget")
				if err := os.MkdirAll(packageRoot, 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(packageRoot, "integration.json"), []byte(`{"schema_version":1,"retired":true,"lifecycle":{"handler":"retire.py","instance_config_keys":[],"instance_devices":[]}}`), 0600)
				testkit.WriteFile(t, filepath.Join(packageRoot, "retire.py"), []byte("#!/bin/sh\n"), 0700)
			}
			if mode == "retired-proven" {
				instance.Config["user.subyard.retired_integration.widget"], instance.LocalConfig["user.subyard.retired_integration.widget"] = "true", "true"
			}
			program, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			settingsPath := filepath.Join(execution.loaded.Context.Paths.ConfigHome, "yards/default/config.env")
			body, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			profilePath := filepath.Join(root, "config/yards/profiles/migration-tools.env")
			if err := os.MkdirAll(filepath.Dir(profilePath), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, profilePath, []byte("ALLOWS_CODING_TOOLS=true\n"), 0600)
			selection := ""
			if nativePending {
				selection = "sample"
			}
			integrations := ""
			if retired {
				integrations = "widget"
			}
			for key, value := range map[string]string{"YARD_TEMPLATE": "migration-tools", "CODING_TOOL_INTEGRATIONS": integrations, "ENVIRONMENT_PROFILES": selection} {
				body, err = config.EditPersistentAssignmentContent(settingsPath, body, key, &value)
				if err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, settingsPath, body, 0600)
			testkit.WriteFile(t, filepath.Join(root, "scripts/06-network.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			testkit.WriteFile(t, filepath.Join(root, "scripts/lifecycle-guard.sh"), []byte("#!/bin/sh\ncase \"$1\" in start) printf running > \"$STATE/physical-status\";; stop) printf stopped > \"$STATE/physical-status\";; *) exit 90;; esac\n"), 0700)
			program.options.Stdin = strings.NewReader("yes\n")
			program.openTerminal = func() (*os.File, error) { return nil, errors.New("test uses explicit prompt") }
			request := releasetransition.ProcessRequest{ConfigHome: execution.loaded.Context.Paths.ConfigHome}
			register := program.registeredYardMigration(request, false, "default")
			registration, err := register.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if err != nil {
				t.Fatal(err)
			}
			if !nativePending {
				if err := register.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
					t.Fatal(err)
				}
			}
			reconciler := &materializedConfigActivationReconciler{cli: program, configHome: request.ConfigHome, yard: "default"}
			releases := releasetransition.ReleasePair{From: "release-a", Target: "release-b"}
			view := preparedActivationSnapshotFixture{home: request.ConfigHome}
			owner := program.registeredYardMigration(request, true, "default").(*migration.RegisteredYardMigration)
			_, err = owner.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
			if err != nil {
				t.Fatal(err)
			}
			sourceView := releasetransition.V2SourceActivationView{Settings: view, RegisteredYards: map[string]releasetransition.Fingerprint{}, GuestDiskTmp: map[string]releasetransition.Fingerprint{}}
			if nativePending {
				sourceView.RegisteredYards[register.ID()] = registration.Desired
			}
			projected, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, sourceView)
			if retired {
				if err != nil {
					t.Fatal("prepare", err)
				}
				reminder := "retired integration widget may still be installed in yard default; run: yard -Y default start; then run yard migrate"
				if slices.Contains(projected.Consequences, reminder) != (mode == "retired-stopped") {
					t.Fatalf("update check consequences=%q", projected.Consequences)
				}
				return
			}
			if err != nil {
				t.Fatal("prepare", err)
			}
			if !reconciler.projectedUnavailable["default"].absent && len(reconciler.projectedUnavailable) == 0 {
				t.Fatal("fixture did not capture unavailable integrations")
			}
			if nativePending {
				if err := register.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := owner.Observe(context.Background(), releases, releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal(err)
			}
			if mode != "native-stopped" {
				if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
					t.Fatal(err)
				}
			}
			if native.markerPower != "" {
				t.Fatal("stopped yard was booted or falsely marked", native.markerPower)
			}
			_, err = reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})

			if err != nil {
				t.Fatal("completed owned migration invalidated prepared availability", err)
			}
			for _, change := range []string{"uuid", "config", "device", "intent", "power", "missing", "private-input"} {
				if change == "private-input" && !nativePending {
					continue
				}
				current, err := native.Instance(context.Background(), instance.Project, instance.Name)
				if err != nil {
					t.Fatal(err)
				}
				saved := native.Instances[instance.Project+"/"+instance.Name]
				switch change {
				case "uuid":
					current.Config["volatile.uuid"] = "foreign"
				case "config":
					current.Config["security.privileged"] = "true"
				case "device":
					current.LocalDevices["foreign"] = map[string]string{"type": "disk", "source": "/foreign", "path": "/foreign"}
				case "intent":
					current.Config["user.subyard.desired_power"], current.LocalConfig["user.subyard.desired_power"] = "running", "running"
				case "power":
					testkit.WriteFile(t, status, []byte("running"), 0600)
				case "private-input":
					testkit.WriteFile(t, filepath.Join(request.ConfigHome, "host-id"), []byte("different-owner\n"), 0600)
				}
				native.Instances[instance.Project+"/"+instance.Name] = current
				if change == "missing" {
					delete(native.Instances, instance.Project+"/"+instance.Name)
				}
				if _, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{}); err == nil {
					t.Fatal("unapproved state reused completion proof", change)
				}
				native.Instances[instance.Project+"/"+instance.Name] = saved
				if change == "power" {
					testkit.WriteFile(t, status, []byte("stopped"), 0600)
				}
			}
		})
	}
}

func TestPreparedActivationRejectsWrongBoundOwnerRegistrationAndOverrides(t *testing.T) {
	for _, field := range []string{"registration", "overrides"} {
		t.Run(field, func(t *testing.T) {
			fixture := newSourceScopeFixture(t)
			registration := filepath.Join(fixture.configHome, "yards", "e2e-yard", "config.env")
			if err := os.MkdirAll(filepath.Dir(registration), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, registration, []byte("YARD_TEMPLATE=test-vms\n"), 0600)
			final := []byte("YARD_TEMPLATE='test-vms'\n")
			view := preparedActivationSnapshotFixture{home: fixture.configHome, names: []string{"e2e-yard"}, snapshots: map[string]config.PersistentFileSnapshot{registration: {Exists: true, Content: final}}}
			owner := newInterruptedRenameOwner(t, fixture.root, fixture.configHome)
			before, err := owner.Prepare(context.Background(), view)
			if err != nil {
				t.Fatal(err)
			}
			if before.Registration != releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(final))) {
				t.Fatal("owner did not bind final prepared settings")
			}
			if field == "registration" {
				before.Registration = releasetransition.Fingerprint(strings.Repeat("1", 64))
			} else {
				before.Overrides = releasetransition.Fingerprint(strings.Repeat("2", 64))
			}
			draft, err := fixture.program.prepareSourceActivationDraft(fixture.configHome, releasetransition.V2SourceActivationView{Settings: view, OwnerRegistration: &before})
			if draft != nil {
				t.Fatal("invalid owner binding returned a draft")
			}
			if err == nil {
				t.Fatal("wrong prepared owner binding was accepted")
			}
			if _, err := os.Stat(owner.current); !os.IsNotExist(err) {
				t.Fatal("invalid owner binding published registration", err)
			}
			if len(fixture.boundary.applied) != 0 {
				t.Fatal("invalid owner binding reached activation writes")
			}
			journal, err := fixture.store.ReadCurrentJournal()
			if err != nil || journal.Exists {
				t.Fatal("invalid owner binding published checkpoint", err)
			}
		})
	}
}

type preparedActivationSnapshotFixture struct {
	home      string
	snapshots map[string]config.PersistentFileSnapshot
	names     []string
}

type unreadPreparedActivationFixture struct {
	preparedActivationSnapshotFixture
	reads int
}

func (view *unreadPreparedActivationFixture) ReadPreparedInput(path string) (config.PersistentFileSnapshot, bool, error) {
	if _, exists := view.snapshots[path]; exists {
		view.reads++
		return config.PersistentFileSnapshot{}, true, errors.New("unrelated captured payload must not be read")
	}
	return config.PersistentFileSnapshot{}, false, nil
}

func TestPreparedActivationDoesNotReadUnrelatedLargeInventory(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	testkit.WriteFile(t, filepath.Join(fixture.root, "config", "projects-changed.sh"), []byte("#!/bin/sh\n"), 0755)
	fixture.program.options.InitPlatform = nil
	fixture.program.options.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	fixture.program.options.Executor = nil
	view := &unreadPreparedActivationFixture{preparedActivationSnapshotFixture: preparedActivationSnapshotFixture{home: fixture.configHome, snapshots: map[string]config.PersistentFileSnapshot{}}}
	payload := []byte(strings.Repeat("unrelated", 1024))
	for index := 0; index < 5000; index++ {
		path := filepath.Join(fixture.configHome, "secrets", "legacy", "qa-pool", fmt.Sprintf("archive-%d", index))
		view.snapshots[path] = config.PersistentFileSnapshot{Exists: true, Content: payload}
	}
	if len(view.snapshots)*len(payload) <= 32<<20 {
		t.Fatal("fixture does not exceed the removed aggregate bound")
	}
	reconciler := fixture.reconciler(false).materializedConfigActivationReconciler
	releases := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
	native, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	projected, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, releasetransition.V2SourceActivationView{Settings: view})
	if err != nil || projected.Desired != native.Desired || view.reads != 0 {
		t.Fatalf("unused inventory changed assessment or was read: projected=%s native=%s reads=%d err=%v", projected.Desired, native.Desired, view.reads, err)
	}
}

func TestPreparedActivationPreservesUncapturedNativeInstructionSymlinkAndLargeAsset(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	testkit.WriteFile(t, filepath.Join(fixture.root, "config", "projects-changed.sh"), []byte("#!/bin/sh\n"), 0755)
	fixture.program.options.InitPlatform = nil
	fixture.program.options.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	fixture.program.options.Executor = nil
	target := filepath.Join(fixture.configHome, "instructions-large.md")
	link := filepath.Join(fixture.configHome, "instructions.md")
	payload := []byte(strings.Repeat("Native instructions.\n", 450000))
	if len(payload) <= 8<<20 {
		t.Fatal("fixture does not exceed persistent settings size")
	}
	testkit.WriteFile(t, target, payload, 0600)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(fixture.configHome, "config.env")
	settings, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, host, append(settings, []byte("HOST_CLAUDE_MD='"+link+"'\n")...), 0600)
	reconciler := fixture.reconciler(false).materializedConfigActivationReconciler
	releases := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
	native, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	view := preparedActivationSnapshotFixture{home: fixture.configHome}
	projected, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, releasetransition.V2SourceActivationView{Settings: view})
	if err != nil || projected.Desired != native.Desired {
		t.Fatalf("native asset reader policy changed: projected=%s native=%s err=%v", projected.Desired, native.Desired, err)
	}
	draft, err := fixture.program.prepareSourceActivationDraft(fixture.configHome, releasetransition.V2SourceActivationView{Settings: view})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := draft.readAsset(config.MaterializedAsset{Source: link}); err == nil {
		t.Fatal("strict uncaptured asset accepted a symlink")
	}
	if actual, err := draft.readAsset(config.MaterializedAsset{Source: target}); err != nil || len(actual) != len(payload) {
		t.Fatal("large native regular asset was narrowed", err)
	}
}

func (view preparedActivationSnapshotFixture) ListYards() ([]string, error) {
	return slices.Clone(view.names), nil
}
func (view preparedActivationSnapshotFixture) PreparedInputPaths() ([]string, error) {
	var paths []string
	for path := range view.snapshots {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths, nil
}
func (view preparedActivationSnapshotFixture) ReadSnapshot(path string) (config.PersistentFileSnapshot, error) {
	if snapshot, ok := view.snapshots[path]; ok {
		return snapshot, nil
	}
	return config.ReadPersistentFileSnapshot(view.home, path)
}

func (view preparedActivationSnapshotFixture) ReadPreparedInput(path string) (config.PersistentFileSnapshot, bool, error) {
	snapshot, exists := view.snapshots[path]
	return snapshot, exists, nil
}

func TestPreparedMaterializedActivationMatchesNativeImportedAssetsAndNewYard(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "injected-platform", true: "native-platform"}[native], func(t *testing.T) {
			checkPreparedMaterializedActivation(t, native)
		})
	}
}

func checkPreparedMaterializedActivation(t *testing.T, native bool) {
	t.Helper()
	fixture := newSourceScopeFixture(t)
	testkit.WriteFile(t, filepath.Join(fixture.root, "config", "projects-changed.sh"), []byte("#!/bin/sh\n"), 0755)
	declareCLIInstructionFixture(t, fixture.root, "claude", "HOST_CLAUDE_MD", ".claude/CLAUDE.md", "Claude instructions")
	catalog, catalogErr := config.LoadCatalog(fixture.root)
	if catalogErr != nil {
		t.Fatal(catalogErr)
	}
	fixture.program.catalog = catalog
	if native {
		fixture.program.options.InitPlatform = nil
	}
	fixture.program.options.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	fixture.program.options.Executor = nil
	reconciler := fixture.reconciler(false).materializedConfigActivationReconciler
	releases := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
	before, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	platform := fixture.program.options.InitPlatform
	if native {
		fixture.program.options.InitPlatform = newInitPlatformFixture()
	} else {
		fixture.program.options.InitPlatform = nil
	}
	otherPlatform, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	fixture.program.options.InitPlatform = platform
	if err != nil || otherPlatform.Desired != before.Desired {
		t.Fatalf("platform injection changed native desired fingerprint: injected=%s native=%s err=%v", otherPlatform.Desired, before.Desired, err)
	}
	hostAsset := filepath.Join(fixture.configHome, "overrides", "host", "agents", "claude", "settings.json")
	newAsset := filepath.Join(fixture.configHome, "yards", "imported-yard", "overrides", "agents", "claude", "settings.json")
	registration := filepath.Join(fixture.configHome, "yards", "imported-yard", "config.env")
	view := preparedActivationSnapshotFixture{home: fixture.configHome, names: []string{"named", "imported-yard"}, snapshots: map[string]config.PersistentFileSnapshot{
		hostAsset:    {Exists: true, Content: []byte(`{"env":{"SCOPE":"default-imported"}}`)},
		newAsset:     {Exists: true, Content: []byte(`{"env":{"SCOPE":"canonical-imported"}}`)},
		registration: {Exists: true, Content: []byte("SSH_PORT=2244\nAGENT_claude_CONFIG='" + newAsset + "'\n")},
	}}
	projected, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, releasetransition.V2SourceActivationView{Settings: view})
	if err != nil {
		t.Fatal(err)
	}
	if projected.Desired == before.Desired {
		t.Fatal("projection did not bind imported assets and new yard")
	}
	if _, err := os.Stat(registration); !os.IsNotExist(err) {
		t.Fatal("prospective assessment published registration", err)
	}
	if got, err := os.ReadFile(hostAsset); err != nil || string(got) == string(view.snapshots[hostAsset].Content) {
		t.Fatal("prospective assessment wrote imported bytes", err)
	}
	for path, snapshot := range view.snapshots {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, snapshot.Content, 0600)
	}
	after, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if projected.Desired != after.Desired {
		t.Fatalf("projected %s != native %s", projected.Desired, after.Desired)
	}
	if len(fixture.boundary.applied) != 0 {
		t.Fatal("desired comparison performed activation writes")
	}
	profile := fixture.program.profileActivationReconciler(fixture.request, "sample-runtime").(*profileRuntimeActivationReconciler)
	stableBefore, err := profile.desired(releases)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, hostAsset, []byte(`{"env":{"SCOPE":"unexpected-drift"}}`), 0600)
	drift, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if drift.Desired == projected.Desired {
		t.Fatal("unexpected asset drift retained projected authority")
	}
	fixture.program.options.Incus.(*testkit.Incus).Instances["subyard-imported-yard/yard-imported-yard"] = ports.InstanceInfo{Project: "subyard-imported-yard", Name: "yard-imported-yard", Status: "Running"}
	if _, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{}); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("unapproved backend availability was accepted", err)
	}
	if len(fixture.boundary.applied) != 0 {
		t.Fatal("backend drift reached config activation writes")
	}
	journal, err := fixture.store.ReadCurrentJournal()
	if err != nil || journal.Exists {
		t.Fatal("adapter assessment published a journal", err)
	}
	stableAfter, err := profile.desired(releases)
	if err != nil || stableAfter != stableBefore {
		t.Fatal("static profile Desired changed", err)
	}
}

func TestPreparedActivationOmitsDisabledIntegrationScope(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "injected-platform", true: "native-platform"}[native], func(t *testing.T) {
			fixture := newSourceScopeFixture(t)
			if native {
				fixture.program.options.InitPlatform = nil
			}
			fixture.program.options.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
			fixture.program.options.Executor = nil
			profile := filepath.Join(fixture.root, "config/yards/profiles/restricted.env")
			if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, profile, []byte("ALLOWS_CODING_TOOLS=false\n"), 0o600)
			for _, name := range []string{"default", "named"} {
				path := filepath.Join(fixture.configHome, "yards", name, "config.env")
				before, err := os.ReadFile(path)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, append(before, []byte("YARD_TEMPLATE=restricted\n")...), 0o600)
				loaded, err := fixture.program.resolveReleaseTransitionContext(context.Background(), name, fixture.configHome)
				if err != nil || loaded.Integrations.AllowsCodingTools {
					t.Fatalf("restricted fixture did not disable coding tools: %s %v", name, err)
				}
			}
			// A disabled owner must not read or require the native integration hook.
			if err := os.Remove(filepath.Join(fixture.root, "config/projects-changed.sh")); err != nil {
				t.Fatal(err)
			}
			reconciler := fixture.reconciler(false).materializedConfigActivationReconciler
			releases := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
			fresh, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
			if err != nil {
				t.Fatal(err)
			}
			view := preparedActivationSnapshotFixture{home: fixture.configHome, names: []string{"named"}}
			prepared, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, releasetransition.V2SourceActivationView{Settings: view})
			if err != nil || prepared.Desired != fresh.Desired || len(fixture.boundary.applied) != 0 {
				t.Fatalf("disabled scope changed desired identity or performed writes: prepared=%s fresh=%s err=%v", prepared.Desired, fresh.Desired, err)
			}
		})
	}
}

// Release activation applies yard settings without refusing ignored
// profile-only operator assignments of any yard.
func TestPreparedActivationIgnoresProfileOnlyOperatorAssignments(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	fixture.program.options.InitPlatform = nil
	fixture.program.options.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	fixture.program.options.Executor = nil
	host := filepath.Join(fixture.configHome, "config.env")
	settings, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, host, append(settings, []byte("YARD_CAPS=\nYARD_DEVICES=fuse\n")...), 0o600)
	named := filepath.Join(fixture.configHome, "yards", "named", "config.env")
	settings, err = os.ReadFile(named)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, named, append(settings, []byte("YARD_MOUNTS=cache:/srv/cache:rw:0755\n")...), 0o600)
	reconciler := fixture.reconciler(false).materializedConfigActivationReconciler
	reconciler.yard, reconciler.allLocal = "", true
	releases := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
	if _, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, releasetransition.V2SourceActivationView{Settings: preparedActivationSnapshotFixture{home: fixture.configHome}}); err != nil {
		t.Fatalf("activation refused ignored profile-only assignments: %v", err)
	}
}
