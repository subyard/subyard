package cli

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/sshagentruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

func teardownRecordLoaded(root string) config.Loaded {
	return config.Loaded{Context: domain.Context{YardName: "default", AccessKind: domain.AccessLocal, YardKind: domain.YardContainer, YardInstanceName: "yard", IncusProject: "project", IncusBridge: "bridge", SSHHost: "yard", DevUser: "dev", SSHPort: 2222, DevUID: 1000, ShiftMode: "shift", Paths: domain.RuntimePaths{RepositoryRoot: root, ConfigDir: filepath.Join(root, "config"), OperatorHome: filepath.Join(root, "operator"), ConfigHome: filepath.Join(root, "settings"), DataHome: filepath.Join(root, "data"), StoragePath: filepath.Join(root, "storage"), HostBase: filepath.Join(root, "host"), StateDir: filepath.Join(root, "state")}}, Environment: map[string]string{"SRV_POOL": "pool", "SRV_VOLUME": "volume", "YARD_RUNTIME_ROOT": filepath.Join(root, "runtime-\xff")}}
}

func teardownRecordFixture(t *testing.T) (*teardownExecution, config.Loaded, *CLI) {
	t.Helper()
	root := filepath.Join(testkit.TempDir(t), "repository-\xff")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	loaded := teardownRecordLoaded(root)
	manager := sshagentruntime.Manager{Config: sshagentruntime.Config{Directory: sshagentruntime.Directory(loaded.Context.Paths.DataHome, loaded.Context.YardName)}}
	agent, err := manager.PrepareLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	value := "original-\xff\xfe"
	resource := ports.TeardownResource{Kind: "volume", Name: value, Pool: "pool", Binding: value, SnapshotCount: 3, Path: filepath.Join(root, value), UsedBy: []string{}, Bindings: []string{value}, Owner: &ports.InstallationResourceOwner{UID: "1000", DataHome: loaded.Context.Paths.DataHome, ConfigHome: loaded.Context.Paths.ConfigHome, Context: &loaded.Context}}
	artifact := ports.TeardownArtifact{Path: filepath.Join(root, value), Binding: value, Entries: []ports.ArtifactEntry{{Path: ".", Mode: 0o100600, Size: 12, Modified: 13, Device: 14, Inode: 15, UID: 16, GID: 17, Changed: 18, Link: value}}, Opaque: true, Exclusions: []string{value}, Category: value, Preserved: []string{}, Resource: &resource, Scoped: true, OwnedPaths: []string{value}, Namespaces: []ports.ArtifactEntry{}, Mutable: true}
	service := uninstall.RecoveryService{Scope: value, Units: []string{value}, Artifacts: []ports.TeardownArtifact{artifact}}
	snapshot := &teardownSnapshot{Resources: []ports.TeardownResource{resource}, Artifacts: []ports.TeardownArtifact{artifact}, Registered: []domain.Context{loaded.Context}, Services: []uninstall.RecoveryService{service}, State: ports.ReconcileState{ProjectConfig: map[string]string{value: value}, ProfileConfig: map[string]string{}, ProfileDevices: map[string]map[string]string{"empty": {}, "nil": nil, value: {value: value}}, Instance: ports.InstanceInfo{Name: value, Project: value, Type: domain.YardContainer, Status: "Running", Config: map[string]string{value: value}, Devices: map[string]map[string]string{value: {value: value}}, LocalConfig: map[string]string{}, LocalDevices: map[string]map[string]string{}}, HostPoolFound: true, HostNetworkFound: true, ProjectFound: true, ProfileFound: true, InstanceFound: true, VolumeFound: true}, Agent: agent, AgentBinding: agent.Binding(), AgentState: agent.State()}
	live := *snapshot
	live.Artifacts = []ports.TeardownArtifact{}
	network := &yardnetwork.RemovalPlan{Yard: networkYard(loaded.Context), Stored: yardnetwork.StoredPolicy{Policy: yardnetwork.Policy{Schema: 1, Isolation: true, Links: []yardnetwork.Link{{A: "default", B: "peer"}}, Revision: 4, AppliedRevision: 3, AppliedIsolation: true, PendingStart: []string{}, Removing: []string{"default"}, Bindings: []yardnetwork.Binding{{Yard: networkYard(loaded.Context), IPv4: "10.0.0.2", MAC: "00:11:22:33:44:55", OriginalNIC: map[string]string{value: value}, OriginalAccess: value, ApprovedIngress: []yardnetwork.ApprovedIngress{{Device: "proxy", Protocol: "tcp", Listen: "tcp:127.0.0.1:8080", GuestPort: 80}}}}}, Content: value, ETag: value}, Cleanup: true, Consequences: []string{value}}
	tree := uninstall.Tree(artifact)
	execution := &teardownExecution{teardownOptions: teardownOptions{holdShared: true, revokeAgent: true}, RecoveryPlan: uninstall.RecoveryPlan{Schema: 1, Installation: uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), DataHome: loaded.Context.Paths.DataHome, ConfigHome: loaded.Context.Paths.ConfigHome, RuntimeRoot: filepath.Join(root, "runtime"), CacheRoot: filepath.Join(root, "cache"), BinDir: filepath.Join(root, "bin"), ShellFiles: []string{value}, CacheFiles: artifact.Entries}, Stage: uninstall.RecoveryPrepared, Yards: []uninstall.RecoveryYard{{Context: loaded.Context, Environment: structuredCommandContext(loaded), Snapshot: snapshot, Network: network}}, Trees: []uninstall.Tree{tree}, Launchers: []uninstall.Tree{}, Startup: []uninstall.StartupEdit{{Before: tree, Blocks: []string{value}, Pending: &tree, Temporary: value}}, OwnedGitDirectories: []string{value}, Services: []uninstall.RecoveryService{service}, Preserved: []string{}, SourceRoot: root, SourceAssets: []uninstall.Tree{tree}, AssetsReady: true, Shared: []ports.TeardownResource{resource}, OwnerLock: &tree, EpochBefore: 19, ProfileChecks: []uninstall.RecoveryProfileCheck{{Profile: "generic", Hook: "hook.sh", Environment: map[string]string{value: value}}}}, changed: true, physicalChanged: true, liveSnapshot: &live, RecoveryBinding: &tree, Assessed: true, resumed: true, ManagedRoots: []string{value}, PreservedRoots: []string{}, MutableRoots: []string{value}}
	execution.activeYard = &execution.Yards[0]
	return execution, loaded, &CLI{options: Options{RepositoryRoot: root}}
}

func TestOwnerTeardownRecordPreservesOriginalPlanAndAssociations(t *testing.T) {
	original, loaded, cli := teardownRecordFixture(t)
	raw, err := captureOwnerTeardownRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restoreOwnerTeardownRecord(raw, loaded, cli)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Fatal("original teardown fields changed during restoration")
	}
	if restored.activeYard != &restored.Yards[0] || restored.liveSnapshot == restored.Yards[0].Snapshot {
		t.Fatal("original active/live association changed")
	}
	if restored.snapshot().Agent == nil || restored.snapshot().Agent == original.snapshot().Agent || restored.snapshot().Agent.Binding() != original.snapshot().AgentBinding || restored.snapshot().Agent.State() != original.snapshot().AgentState {
		t.Fatal("original lock was not independently reconstructed")
	}
	again, err := captureOwnerTeardownRecord(restored)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("nondeterministic round trip: %v", err)
	}
	original.liveSnapshot = original.snapshot()
	raw, err = captureOwnerTeardownRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	restored, err = restoreOwnerTeardownRecord(raw, loaded, cli)
	if err != nil || restored.liveSnapshot != restored.snapshot() {
		t.Fatalf("shared snapshot identity lost: %v", err)
	}
}

func TestOwnerTeardownRecordUsesLogicalHookLabelsAndRetainsExecutionFD(t *testing.T) {
	original, loaded, cli := teardownRecordFixture(t)
	root := loaded.Context.Paths.RepositoryRoot
	// Reuse the complete owning loader bootstrap rather than a partial set of
	// required context defaults. Rebind only this fixture-authored source root.
	if err := os.MkdirAll(filepath.Join(root, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	bootstrap, _, _ := nativeFixture(t)
	for _, name := range []string{"incus.project.env", "subyard.env", "host.env", "agents.env", "ports.env"} {
		raw, err := os.ReadFile(filepath.Join(bootstrap, "config", name))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "config", name), []byte(strings.ReplaceAll(string(raw), bootstrap, root)), 0600)
	}
	profile := filepath.Join(root, "config/profiles/fixture")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(profile, "profile.json"), []byte(`{"schema_version":1,"guest_environment":{"handler":"guest.sh"}}`), 0600)
	testkit.WriteFile(t, filepath.Join(profile, "guest.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	sourceDir := filepath.Join(root, "config/agents/sample")
	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(sourceDir, "integration.json"), []byte(`{"schema_version":2,"artifacts":[{"id":"config","kind":"config","source":"config.json","destination":".sample/config.json","format":"destination"}]}`), 0600)
	for _, name := range []string{"config.json", "second.json"} {
		testkit.WriteFile(t, filepath.Join(sourceDir, name), []byte("{}\n"), 0600)
	}
	if err := os.MkdirAll(filepath.Join(loaded.Context.Paths.ConfigHome, "yards", "second"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(loaded.Context.Paths.ConfigHome, "yards", "second", "config.env"), []byte("AGENT_sample_CONFIG=\"${SUBYARD_CONFIG_DIR}/agents/sample/second.json\"\n"), 0600)
	// Keep the fixture's native snapshots; independently load checked package
	// defaults/overrides for each yard and descriptor without effect execution.
	inputs := func(base config.Loaded, source string) config.Loaded {
		t.Helper()
		checked, err := config.Load(config.LoadOptions{RepositoryRoot: source, YardName: base.Context.YardName, OperatorHome: base.Context.Paths.OperatorHome, DisablePrivate: true, Environment: map[string]string{"SUBYARD_HOME": base.Context.Paths.DataHome, "SUBYARD_CONFIG_HOME": base.Context.Paths.ConfigHome, "SUBYARD_OPERATOR_HOME": base.Context.Paths.OperatorHome, "CODING_TOOL_INTEGRATIONS": "sample"}})
		if err != nil {
			t.Fatal("checked package inputs", err)
		}
		base.Catalog = checked.Catalog
		base.Settings = checked.Settings
		base.Environment = checked.Environment
		if trace := base.Settings["AGENT_sample_CONFIG"]; trace.Kind != config.SettingFile || trace.Type != config.SettingRegularFilePath {
			t.Fatal("fixture lacks checked typed package authority")
		}
		return base
	}
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
	foreign, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	firstRoot := fmt.Sprintf("/proc/self/fd/%d", first.Fd())
	secondRoot := fmt.Sprintf("/proc/self/fd/%d", second.Fd())
	// Child readers can name this process explicitly for inherited descriptors.
	loaded = inputs(loaded, fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), first.Fd()))
	original.retainOwnerSourceSettings(loaded)
	original.Yards[0].Environment = structuredCommandContext(loaded)
	secondYard := loaded
	secondYard.Context.YardName = "second"
	secondYard.Context.YardInstanceName = "second"
	secondYard = inputs(secondYard, firstRoot)
	original.retainOwnerSourceSettings(secondYard)
	if secondYard.Environment["AGENT_sample_CONFIG"] == loaded.Environment["AGENT_sample_CONFIG"] {
		t.Fatal("distinct yard inputs not established")
	}
	original.Yards = append(original.Yards, uninstall.RecoveryYard{Context: secondYard.Context, Environment: structuredCommandContext(secondYard), Snapshot: &teardownSnapshot{}})
	original.activeYard = &original.Yards[0]
	const key = "SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS"
	originalHook := original.Yards[0].Environment[key]
	if !strings.Contains(originalHook, fmt.Sprintf("/fd/%d/", first.Fd())) {
		t.Fatal("fixture did not bind the first descriptor")
	}
	original.snapshot().Registered[0].Paths.RepositoryRoot = firstRoot
	original.snapshot().Registered[0].Paths.ConfigDir = filepath.Join(firstRoot, "config")
	raw, err := captureOwnerTeardownRecord(original, firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	var descriptor ownerTeardownRecord
	if err := nativejson.DecodeLosslessNativeJSON(raw, &descriptor, ownerTeardownRecordLimit); err != nil {
		t.Fatal(err)
	}
	if descriptor.Snapshots[0].Registered[0].Paths.RepositoryRoot != root || descriptor.Snapshots[0].Registered[0].Paths.ConfigDir != filepath.Join(root, "config") || original.snapshot().Registered[0].Paths.RepositoryRoot != firstRoot {
		t.Fatal("capture lost logical peer labels or changed live peer handles")
	}
	if original.Yards[0].Environment[key] != originalHook {
		t.Fatal("capture changed the live execution environment")
	}
	fresh := inputs(loaded, secondRoot)
	freshSecond := inputs(secondYard, secondRoot)
	cli.options.RepositoryRoot = secondRoot
	restored, err := restoreOwnerTeardownRecord(raw, fresh, cli, freshSecond)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Yards[0].Environment[key] != structuredCommandContext(fresh)[key] {
		t.Fatal("restoration lost the current executable descriptor")
	}
	for i, checked := range []config.Loaded{fresh, freshSecond} {
		if restored.Yards[i].Environment["AGENT_sample_CONFIG"] != structuredCommandContext(checked)["AGENT_sample_CONFIG"] {
			t.Fatal("yard source lost its own checked physical value")
		}
	}
	wrongType := fresh
	wrongType.Settings = maps.Clone(fresh.Settings)
	trace := wrongType.Settings["AGENT_sample_CONFIG"]
	trace.Kind = config.SettingScalar
	trace.Type = config.SettingString
	wrongType.Settings["AGENT_sample_CONFIG"] = trace
	if _, err := restoreOwnerTeardownRecord(raw, wrongType, cli, freshSecond); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("unchecked source type gained label authority", err)
	}
	for _, peers := range [][]config.Loaded{nil, {freshSecond, freshSecond}, {fresh}} {
		if _, err := restoreOwnerTeardownRecord(raw, fresh, cli, peers...); !errors.Is(err, domain.ErrPlanStale) {
			t.Fatal("missing/duplicate/conflicting peer admitted", err)
		}
	}
	again, err := captureOwnerTeardownRecord(restored, secondRoot)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("descriptor alias changed original metadata", err)
	}
	for _, drift := range []string{"foreign descriptor", "hook target", "unrelated environment", "nonactive hook target", "foreign source FD", "foreign source PID", "source sibling", "source target", "source presence", "source type", "nonactive source target"} {
		t.Run(drift, func(t *testing.T) {
			var record ownerTeardownRecord
			if err := nativejson.DecodeLosslessNativeJSON(raw, &record, ownerTeardownRecordLimit); err != nil {
				t.Fatal(err)
			}
			index := 0
			if drift == "nonactive hook target" || drift == "nonactive source target" {
				index = 1
			}
			environment := record.Yards[index].Environment.restore()
			switch drift {
			case "foreign descriptor":
				environment[key] = strings.Replace(environment[key], root, fmt.Sprintf("/proc/self/fd/%d", foreign.Fd()), 1)
			case "hook target", "nonactive hook target":
				environment[key] += ".changed"
			case "foreign source FD":
				environment["AGENT_sample_CONFIG"] = strings.Replace(environment["AGENT_sample_CONFIG"], root, fmt.Sprintf("/proc/self/fd/%d", foreign.Fd()), 1)
			case "foreign source PID":
				environment["AGENT_sample_CONFIG"] = strings.Replace(environment["AGENT_sample_CONFIG"], root, fmt.Sprintf("/proc/%d/fd/%d", os.Getpid()+1, first.Fd()), 1)
			case "source sibling":
				environment["AGENT_sample_CONFIG"] = strings.Replace(environment["AGENT_sample_CONFIG"], root, root+"-other", 1)
			case "source target", "nonactive source target":
				environment["AGENT_sample_CONFIG"] += ".changed"
			case "source presence":
				delete(environment, "AGENT_sample_CONFIG")
			case "source type":
				environment["AGENT_sample_CONFIG"] = "literal"
			case "unrelated environment":
				environment["SRV_POOL"] = "changed"
			}
			record.Yards[index].Environment = teardownMap(environment)
			changed, err := nativejson.EncodeLosslessNativeJSON(record, ownerTeardownRecordLimit)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restoreOwnerTeardownRecord(changed, fresh, cli, freshSecond); err == nil || drift != "nonactive hook target" && drift != "nonactive source target" && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("changed original hook/environment admitted", err)
			}
		})
	}
}

func TestOwnerTeardownRecordLosslessAndNoMutationAlias(t *testing.T) {
	original, loaded, cli := teardownRecordFixture(t)
	raw, err := captureOwnerTeardownRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restoreOwnerTeardownRecord(raw, loaded, cli)
	if err != nil {
		t.Fatal(err)
	}
	value := "original-\xff\xfe"
	if restored.snapshot().State.ProjectConfig[value] != value || restored.snapshot().Artifacts[0].Entries[0].Link != value || restored.networkRemoval().Stored.Policy.Bindings[0].OriginalNIC[value] != value || restored.ManagedRoots[0] != value {
		t.Fatal("nonUTF8 bytes changed")
	}
	restored.snapshot().State.ProjectConfig[value] = "mutated"
	restored.snapshot().Artifacts[0].Entries[0].Link = "mutated"
	restored.snapshot().Resources[0].Owner.Context.Paths.DataHome = "mutated"
	restored.networkRemoval().Stored.Policy.Bindings[0].OriginalNIC[value] = "mutated"
	restored.ManagedRoots[0] = "mutated"
	if original.snapshot().State.ProjectConfig[value] != value || original.snapshot().Artifacts[0].Entries[0].Link != value || original.snapshot().Resources[0].Owner.Context.Paths.DataHome != loaded.Context.Paths.DataHome || original.networkRemoval().Stored.Policy.Bindings[0].OriginalNIC[value] != value || original.ManagedRoots[0] != value {
		t.Fatal("restored plan aliases original mutable state")
	}
}

func TestOwnerTeardownRecordPreservesMultipleYardsAndLiveOwner(t *testing.T) {
	original, loaded, cli := teardownRecordFixture(t)
	second := loaded
	second.Context.YardName = "second"
	second.Context.YardInstanceName = "yard-second"
	second.Context.IncusProject = "project-second"
	manager := sshagentruntime.Manager{Config: sshagentruntime.Config{Directory: sshagentruntime.Directory(second.Context.Paths.DataHome, second.Context.YardName)}}
	agent, err := manager.PrepareLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := *original.snapshot()
	snapshot.Agent, snapshot.AgentBinding, snapshot.AgentState = agent, agent.Binding(), agent.State()
	network := *original.networkRemoval()
	network.Yard = networkYard(second.Context)
	original.Yards = append(original.Yards, uninstall.RecoveryYard{Context: second.Context, Environment: structuredCommandContext(second), Snapshot: &snapshot, Network: &network})
	original.activeYard = &original.Yards[1]
	original.liveSnapshot = original.Yards[0].Snapshot
	raw, err := captureOwnerTeardownRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restoreOwnerTeardownRecord(raw, second, cli, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, got) || got.activeYard != &got.Yards[1] || got.liveSnapshot != got.Yards[0].Snapshot {
		t.Fatal("multiple-yard snapshot or live lock owner changed")
	}
}

func TestOwnerTeardownRecordRejectsVariantsAndMalformedDescriptors(t *testing.T) {
	original, loaded, cli := teardownRecordFixture(t)
	for _, variant := range []string{"full", "keep-data", "reset-config", "completed-reset", "config-snapshot"} {
		t.Run(variant, func(t *testing.T) {
			copy := *original
			switch variant {
			case "full":
				copy.full = true
			case "keep-data":
				copy.keepData = true
			case "reset-config":
				copy.resetConfig = true
			case "completed-reset":
				copy.completedReset = true
			case "config-snapshot":
				copy.configSnapshot = &teardownConfigSnapshot{}
			}
			if _, err := captureOwnerTeardownRecord(&copy); err == nil {
				t.Fatal("unsupported mode accepted")
			}
		})
	}
	raw, err := captureOwnerTeardownRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string][]byte{"truncated": raw[:len(raw)-1], "trailing": append(bytes.Clone(raw), ' '), "unknown": bytes.Replace(raw, []byte(`"Version":1`), []byte(`"Unknown":null,"Version":1`), 1), "duplicate": bytes.Replace(raw, []byte(`"Version":1`), []byte(`"Version":1,"Version":1`), 1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := restoreOwnerTeardownRecord(invalid, loaded, cli); err == nil {
				t.Fatal("malformed descriptor accepted")
			}
		})
	}
	for _, invalid := range []string{"version", "schema", "stage", "installation root", "full", "active", "snapshot", "missing snapshot", "duplicate map", "lock binding", "network target", "owner context"} {
		t.Run(invalid, func(t *testing.T) {
			var d ownerTeardownRecord
			if err := nativejson.DecodeLosslessNativeJSON(raw, &d, ownerTeardownRecordLimit); err != nil {
				t.Fatal(err)
			}
			switch invalid {
			case "version":
				d.Version = 2
			case "schema":
				d.Schema = 2
			case "stage":
				d.Stage = "unexpected"
			case "installation root":
				d.Installation.DataHome = "/foreign"
			case "full":
				d.Full = true
			case "active":
				d.ActiveYard = 10
			case "snapshot":
				d.Yards[0].Snapshot = 100
			case "missing snapshot":
				d.Yards[0].Snapshot = -1
			case "duplicate map":
				d.Yards[0].Environment.Rows = append(d.Yards[0].Environment.Rows, d.Yards[0].Environment.Rows[0])
			case "lock binding":
				d.Snapshots[0].AgentBinding = "different"
			case "network target":
				d.Yards[0].Network.Yard.Name = "foreign"
			case "owner context":
				d.Yards[0].Context.Paths.DataHome = "/foreign"
			}
			encoded, err := nativejson.EncodeLosslessNativeJSON(d, ownerTeardownRecordLimit)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restoreOwnerTeardownRecord(encoded, loaded, cli); err == nil {
				t.Fatal("invalid domain descriptor accepted")
			}
		})
	}
}

func TestOwnerTeardownRecordRefusesChangedOriginalContextAndInputs(t *testing.T) {
	original, loaded, cli := teardownRecordFixture(t)
	raw, err := captureOwnerTeardownRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, drift := range []string{"source root", "config root", "data root", "yard identity", "environment"} {
		t.Run(drift, func(t *testing.T) {
			changed := loaded
			switch drift {
			case "source root":
				changed.Context.Paths.RepositoryRoot += "-changed"
			case "config root":
				changed.Context.Paths.ConfigHome += "-changed"
			case "data root":
				changed.Context.Paths.DataHome += "-changed"
			case "yard identity":
				changed.Context.YardInstanceName = "different"
			case "environment":
				changed.Environment = map[string]string{"SRV_POOL": "different"}
			}
			if _, err := restoreOwnerTeardownRecord(raw, changed, cli); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("drift accepted: %v", err)
			}
		})
	}
}

func TestOwnerTeardownRecordDoesNotObserveOrPrepareAgain(t *testing.T) {
	original, loaded, cli := teardownRecordFixture(t)
	raw, err := captureOwnerTeardownRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh Status/PrepareLock would reject this changed control directory.
	directory := sshagentruntime.Directory(loaded.Context.Paths.DataHome, loaded.Context.YardName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreOwnerTeardownRecord(raw, loaded, cli); err != nil {
		t.Fatalf("restore acquired a live baseline: %v", err)
	}
}

func TestOwnerTeardownRecordNilAndAbsentContainers(t *testing.T) {
	raw, err := captureOwnerTeardownRecord(nil)
	if err != nil || raw != nil {
		t.Fatalf("nil capture changed: %v", err)
	}
	if p, err := restoreOwnerTeardownRecord(raw, config.Loaded{}, nil); err != nil || p != nil {
		t.Fatalf("nil restore changed: %v", err)
	}
	original, loaded, cli := teardownRecordFixture(t)
	original.RecoveryPlan = uninstall.RecoveryPlan{Yards: []uninstall.RecoveryYard{{Context: loaded.Context, Environment: structuredCommandContext(loaded), Snapshot: &teardownSnapshot{Resources: []ports.TeardownResource{}, Artifacts: nil, Registered: []domain.Context{}, Services: nil}}}}
	original.activeYard = &original.Yards[0]
	original.liveSnapshot = original.snapshot()
	original.revokeAgent = false
	raw, err = captureOwnerTeardownRecord(original)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restoreOwnerTeardownRecord(raw, loaded, cli)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, got) || got.snapshot().Agent != nil {
		t.Fatal("nil/empty containers or nil agent changed")
	}
}

func TestOwnerTeardownRecordFreshProcessIdentity(t *testing.T) {
	if root := os.Getenv("SUBYARD_TEST_TEARDOWN_RECORD_ROOT"); root != "" {
		var other bytes.Buffer
		if err := gob.NewEncoder(&other).Encode(struct{ Other string }{"unrelated first codec"}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(root, "record"))
		if err != nil {
			t.Fatal(err)
		}
		loaded := teardownRecordLoaded(root)
		p, err := restoreOwnerTeardownRecord(raw, loaded, &CLI{options: Options{RepositoryRoot: root}})
		if err != nil {
			t.Fatal(err)
		}
		again, err := captureOwnerTeardownRecord(p)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatalf("fresh process changed original bytes: %v", err)
		}
		return
	}
	p, loaded, _ := teardownRecordFixture(t)
	raw, err := captureOwnerTeardownRecord(p)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(loaded.Context.Paths.RepositoryRoot, "record"), raw, 0o600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestOwnerTeardownRecordFreshProcessIdentity$", "-test.count=1")
	child.Env = append(os.Environ(), "SUBYARD_TEST_TEARDOWN_RECORD_ROOT="+loaded.Context.Paths.RepositoryRoot)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh process: %v\n%s", err, output)
	}
}
