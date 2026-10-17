package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/uninstall"
	"github.com/Subyard/Subyard/internal/yardnetwork"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerpublication"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/previewroute"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func legacyContinuationFixture(t *testing.T) (reconcileruntime.Runtime, *testkit.Incus) {
	t.Helper()
	root := testkit.TempDir(t)
	yard := domain.Context{YardName: "default", AccessKind: domain.AccessLocal, YardKind: domain.YardContainer, IncusProject: "subyard", YardInstanceName: "yard", IncusBridge: "incusbr0", SSHPort: 2222, Paths: domain.RuntimePaths{OperatorHome: root, ConfigHome: filepath.Join(root, "config"), DataHome: filepath.Join(root, "data"), HostBase: filepath.Join(root, "host")}}
	for _, directory := range []string{yard.Paths.ConfigHome, yard.Paths.DataHome} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	instance := ports.InstanceInfo{Name: "yard", Project: "subyard", Type: domain.YardContainer, Status: "running", Config: map[string]string{"volatile.uuid": "native-uuid", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.desired_power": "running", "user.subyard.name": "default", "user.subyard.bridge": "incusbr0", "boot.autostart": "false"}}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": instance}, Reconcile: ports.ReconcileState{ProjectFound: true, ProfileFound: true, InstanceFound: true, VolumeFound: true, Instance: instance, ProjectConfig: map[string]string{"restricted": "true", "restricted.containers.nesting": "allow", "restricted.containers.privilege": "unprivileged", "restricted.containers.interception": "block", "restricted.devices.disk": "allow", "restricted.devices.unix-char": "allow", "restricted.devices.proxy": "allow"}, ProfileDevices: map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "default"}, "eth0": {"type": "nic", "network": "incusbr0"}}}}
	for _, entry := range []ports.TeardownResource{{Kind: "project", Name: "subyard"}, {Kind: "profile", Name: "default"}, {Kind: "instance", Name: "yard"}, {Kind: "volume", Name: "yard-srv", Pool: "default"}} {
		entry.Binding = strings.Repeat("1", 64)
		incus.TeardownResources = append(incus.TeardownResources, entry)
	}
	runtime := reconcileruntime.Runtime{Yard: yard, Incus: incus, NetworkPolicy: legacyContinuationNetworkFixture{}, Environment: []string{"SUBYARD_HOME=" + yard.Paths.DataHome, "SUBYARD_CONFIG_HOME=" + yard.Paths.ConfigHome, "HOST_MOUNTS=host-memory:/mnt/host/memory:rw:0770", "SHIFT_MODE=shift"}}
	canonicalLegacyInstanceFixture(runtime, incus)
	return runtime, incus
}

// The fixture models the devices authored by the historical default container
// scripts; it does not approve the observed test device inventory.
func canonicalLegacyInstanceFixture(runtime reconcileruntime.Runtime, incus *testkit.Incus) {
	env := environmentMap(runtime.Environment)
	instance := incus.Reconcile.Instance
	instance.Config["security.nesting"] = "true"
	instance.Config["volatile.idmap.base"] = "0"
	instance.Config["volatile.idmap.current"] = `[{"Isuid":true,"Isgid":false,"Hostid":1000000,"Nsid":0,"Maprange":65536},{"Isuid":false,"Isgid":true,"Hostid":1000000,"Nsid":0,"Maprange":65536}]`
	instance.Config["volatile.idmap.next"] = instance.Config["volatile.idmap.current"]
	for key, setting := range map[string]string{"limits.cpu": "LIMITS_CPU", "limits.memory": "LIMITS_MEMORY"} {
		if env[setting] != "" {
			instance.Config[key] = env[setting]
		}
	}
	instance.LocalConfig = maps.Clone(instance.Config)
	pool, volume := runtime.SRVPool, runtime.SRVVolume
	if pool == "" {
		pool = "default"
	}
	if volume == "" {
		volume = "yard-srv"
	}
	instance.LocalDevices = map[string]map[string]string{
		"srv":                {"type": "disk", "pool": pool, "source": volume, "path": "/srv"},
		"subyard-e2e-routes": {"type": "disk", "source": filepath.Join(runtime.Yard.Paths.DataHome, "e2e", "routes"), "path": "/var/lib/subyard/e2e-routes", "readonly": "true"},
		"ssh":                {"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:" + strconv.Itoa(runtime.Yard.SSHPort), "connect": "tcp:127.0.0.1:22"},
	}
	for _, entry := range strings.Fields(env["HOST_MOUNTS"]) {
		parts := strings.Split(entry, ":")
		device := map[string]string{"type": "disk", "source": filepath.Join(runtime.Yard.Paths.HostBase, parts[0]), "path": parts[1]}
		if parts[2] == "ro" {
			device["readonly"] = "true"
		}
		if env["SHIFT_MODE"] != "acl" {
			device["shift"] = "true"
		}
		instance.LocalDevices[parts[0]] = device
	}
	instance.Devices = map[string]map[string]string{}
	for name, device := range incus.Reconcile.ProfileDevices {
		instance.Devices[name] = maps.Clone(device)
	}
	for name, device := range instance.LocalDevices {
		instance.Devices[name] = maps.Clone(device)
	}
	incus.Reconcile.Instance = instance
	incus.Instances = map[string]ports.InstanceInfo{instance.Project + "/" + instance.Name: instance}
}

func TestLegacyContinuationConsentCASAndNarrowAuthority(t *testing.T) {
	runtime, incus := legacyContinuationFixture(t)
	ctx := context.Background()
	continuation, err := assessLegacyContinuation(ctx, runtime, "owner-host", "/runtime", "sealed-history", config.PersistentFileSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	execution := &initExecution{loaded: config.Loaded{Context: runtime.Yard}, legacyContinuation: continuation, hostID: "owner-host"}
	steps := execution.operationSteps()
	if len(steps) == 0 || steps[0].ID != "init.legacy-continuation" {
		t.Fatal("consent plan omitted limited continuation", steps)
	}
	if snapshot, err := readLegacyContinuation(runtime.Yard); err != nil || snapshot.Exists {
		t.Fatal("read-only assessment wrote grant", err)
	}
	if err := continuation.publish(execution); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readLegacyContinuation(runtime.Yard)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := decodeLegacyContinuation(snapshot)
	if err != nil || snapshot.Identity.Mode&0777 != 0600 {
		t.Fatal("unsafe grant", err)
	}
	runtime.LegacyContinuation = &grant
	if err := runtime.ApplyStage(ctx, ports.ReconcileStageProject); err != nil {
		t.Fatal("bounded project apply invoked leaf or failed", err)
	}
	if _, present := incus.Reconcile.ProjectConfig["user.subyard.installation"]; present {
		t.Fatal("continuation stamped installation ownership")
	}
	if err := runtime.Teardown(ctx); err == nil {
		t.Fatal("continuation authorized teardown")
	}
	// Native runtime state and power can change across legitimate boots and stops.
	incus.Reconcile.Instance.Status = "stopped"
	incus.Reconcile.Instance.Config["volatile.last_state.power"] = "STOPPED"
	incus.Reconcile.Instance.LocalConfig["volatile.last_state.power"] = "STOPPED"
	incus.Reconcile.Instance.Config["user.subyard.desired_power"] = "stopped"
	incus.Reconcile.Instance.LocalConfig["user.subyard.desired_power"] = "stopped"
	// Release-link provenance is establishment evidence, not a permanent link pin.
	retry, err := assessLegacyContinuation(ctx, runtime, "owner-host", "/runtime", "new-completed-forward-or-rollback-history", snapshot)
	if err != nil || retry.grant.Provenance != "sealed-history" {
		t.Fatal("legitimate release change invalidated narrow identity", err)
	}
	incus.Reconcile.Instance.Config["volatile.uuid"] = "replacement"
	if err := runtime.ApplyStage(ctx, ports.ReconcileStageProject); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("replacement UUID admitted", err)
	}
	if _, err := assessLegacyContinuation(ctx, runtime, "owner-host", "/runtime", "history", snapshot); err == nil {
		t.Fatal("grant silently rebound to replacement")
	}
	// A competing record wins absence CAS; it must never be overwritten.
	fresh, _ := legacyContinuationFixture(t)
	competing, err := assessLegacyContinuation(ctx, fresh, "owner-host", "/runtime", "history", config.PersistentFileSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	path := legacyContinuationPath(fresh.Yard)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte("foreign record"), 0600)
	if err := competing.publish(&initExecution{loaded: config.Loaded{Context: fresh.Yard}}); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("CAS overwrote competitor", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "foreign record" {
		t.Fatal("competing record changed")
	}
}

func TestLegacyContinuationRefusesForeignIncompleteAndExpandedScope(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*reconcileruntime.Runtime, *testkit.Incus)
	}{
		{"no UUID", func(_ *reconcileruntime.Runtime, i *testkit.Incus) {
			delete(i.Reconcile.Instance.Config, "volatile.uuid")
		}},
		{"missing configured limit", func(r *reconcileruntime.Runtime, _ *testkit.Incus) {
			r.Environment = append(r.Environment, "LIMITS_CPU=2")
		}},
		{"unsupported host role", func(r *reconcileruntime.Runtime, _ *testkit.Incus) {
			r.Environment = append(r.Environment, "ALLOWS_HOST_ACCESS=false")
		}},
		{"unsupported nested policy", func(r *reconcileruntime.Runtime, _ *testkit.Incus) {
			r.Environment = append(r.Environment, "NESTED_E2E_VMS=1")
		}},
		{"unmanaged", func(_ *reconcileruntime.Runtime, i *testkit.Incus) {
			delete(i.Reconcile.Instance.Config, "user.subyard.managed")
		}},
		{"partial ownership", func(_ *reconcileruntime.Runtime, i *testkit.Incus) {
			i.Reconcile.ProjectConfig["user.subyard.data_home"] = "/foreign"
		}},
		{"reporting policy drift", func(r *reconcileruntime.Runtime, i *testkit.Incus) {
			r.Environment = append(r.Environment, "VM_FREE_PAGE_REPORTING=1")
			r.Yard.YardKind = domain.YardVM
			i.Reconcile.Instance.Type = domain.YardVM
			i.ServerInfo.Version = "6.0.6"
		}},
		{"policy drift", func(_ *reconcileruntime.Runtime, i *testkit.Incus) { i.Reconcile.ProjectConfig["restricted"] = "false" }},
		{"foreign NIC", func(_ *reconcileruntime.Runtime, i *testkit.Incus) {
			i.Reconcile.ProfileDevices["eth0"]["network"] = "foreign"
		}},
		{"unknown workload", func(_ *reconcileruntime.Runtime, i *testkit.Incus) {
			i.TeardownResources = append(i.TeardownResources, ports.TeardownResource{Kind: "instance", Name: "foreign", Binding: "unknown"})
		}},
		{"missing volume", func(_ *reconcileruntime.Runtime, i *testkit.Incus) { i.Reconcile.VolumeFound = false }},
		{"remote", func(r *reconcileruntime.Runtime, _ *testkit.Incus) { r.Yard.AccessKind = domain.AccessRemote }},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, incus := legacyContinuationFixture(t)
			test.change(&runtime, incus)
			if _, err := assessLegacyContinuation(context.Background(), runtime, "host", "/runtime", "sealed", config.PersistentFileSnapshot{}); err == nil {
				t.Fatal("unsafe continuation admitted")
			}
			snapshot, _ := readLegacyContinuation(runtime.Yard)
			if snapshot.Exists || len(incus.ConfigUpdates) != 0 {
				t.Fatal("refusal wrote state")
			}
		})
	}
	for _, arguments := range [][]string{{"--continue-legacy", "--reset"}, {"--configs", "--continue-legacy"}, {"--continue-legacy", "--profile", "sample"}} {
		if _, err := parseInitArguments(arguments); err == nil {
			t.Fatal("expanded scope parsed", arguments)
		}
	}
	runtime, _ := legacyContinuationFixture(t)
	cli := &CLI{}
	if err := cli.prepareLegacyContinuation(context.Background(), &initExecution{loaded: config.Loaded{Context: runtime.Yard}, platform: runtime}, initArguments{}); err == nil || !strings.Contains(err.Error(), "--continue-legacy") {
		t.Fatal("plain init silently granted authority", err)
	}
}

func TestLegacyContinuationUnsafeRecordAndChangedOwner(t *testing.T) {
	runtime, _ := legacyContinuationFixture(t)
	original, err := assessLegacyContinuation(context.Background(), runtime, "host", "/runtime", "sealed", config.PersistentFileSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(original.grant)
	path := legacyContinuationPath(runtime.Yard)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, data, 0644)
	snapshot, err := readLegacyContinuation(runtime.Yard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeLegacyContinuation(snapshot); err == nil {
		t.Fatal("public-readable grant admitted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err = readLegacyContinuation(runtime.Yard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assessLegacyContinuation(context.Background(), runtime, "other-host", "/runtime", "sealed", snapshot); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("foreign HostID admitted", err)
	}
	changed := original.grant
	changed.OwnerEpoch++
	payload, _ := json.Marshal(changed)
	testkit.WriteFile(t, path, payload, 0600)
	snapshot, _ = readLegacyContinuation(runtime.Yard)
	if _, err := assessLegacyContinuation(context.Background(), runtime, "host", "/runtime", "sealed", snapshot); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("stale owner epoch admitted", err)
	}
	loaded := config.Loaded{Context: runtime.Yard}
	if err := (&CLI{}).observeFullTeardown(context.Background(), loaded, &fullTeardown{}); err == nil || !strings.Contains(err.Error(), "no full removal ownership") {
		t.Fatal("continuation-only full teardown did not refuse before inventory/deletion", err)
	}
}

func TestLegacyContinuationInstalledSourceAndTypedConsent(t *testing.T) {
	for _, scenario := range []string{"decline", "accept", "implicit-decline", "implicit-accept", "changed-uuid", "changed-config", "changed-record", "changed-resource", "changed-project", "changed-epoch", "changed-host-id", "replaced-registration", "missing-history", "unsafe-provenance"} {
		t.Run(scenario, func(t *testing.T) {
			implicit := strings.HasPrefix(scenario, "implicit-")
			scenario = strings.TrimPrefix(scenario, "implicit-")
			program, execution, incus, path := legacyContinuationInstalledFixture(t, implicit)
			loaded := execution.loaded
			configHome := loaded.Context.Paths.ConfigHome
			var output bytes.Buffer
			program.options.Stdout = &output
			program.options.Stderr = &output
			if scenario == "missing-history" {
				if err := os.Remove(filepath.Join(configHome, "release-transition/v2/journal.json")); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(configHome, "release-transition/v2/journal.json")); !os.IsNotExist(err) {
					t.Fatal("missing-history precondition not established", err)
				}
			}
			if scenario == "unsafe-provenance" {
				testkit.WriteFile(t, filepath.Join(program.options.RepositoryRoot, "runtime-files.sha256"), []byte("invalid\n"), 0600)
			}
			err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{continueLegacy: true})
			if scenario == "unsafe-provenance" || scenario == "missing-history" {
				if err == nil {
					t.Fatal("unsealed source admitted")
				}
				snapshot, readErr := readLegacyContinuation(loaded.Context)
				if readErr != nil || snapshot.Exists || len(incus.ConfigUpdates) != 0 || incus.Reconcile.ProjectConfig["user.subyard.installation"] != "" {
					t.Fatal("unsafe/missing history refusal wrote authority or native state", readErr)
				}
				return
			}
			if err != nil {
				t.Fatal("real installed SourceAdmission prepare", err)
			}
			execution.inputBaseline, err = program.captureOwnerInputs(loaded, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			definition, _ := program.manifest.Lookup("init")
			prepared := &preparedCommand{CLI: program, Definition: definition, Loaded: loaded, Arguments: []string{"--continue-legacy"}, steps: execution.operationSteps, stepsComplete: true, exactState: execution.stateBinding(), assess: func(context.Context) (domain.ActionID, domain.ActionDelta, error) { return execution.actionPlan() }}
			prepared.policy = commandPolicy(definition, loaded.Context, nil, nil)
			prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
				if err := execution.inputBaseline.check(ctx, program); err != nil {
					return "", domain.ActionDelta{}, err
				}
				if err := execution.legacyContinuation.check(ctx, program, execution); err != nil {
					return "", domain.ActionDelta{}, err
				}
				return execution.actionPlan()
			}
			prepared.execute = func(ctx context.Context, _ *application.Orchestrator, _ io.Writer) (domain.AdapterResult, error) {
				if err := execution.legacyContinuation.check(ctx, program, execution); err != nil {
					return domain.AdapterResult{}, err
				}
				return domain.AdapterResult{Schema: 1, OperationID: prepared.Plan.OperationID, Status: "ok"}, execution.legacyContinuation.publish(execution)
			}
			if err := prepared.preparePlan(context.Background()); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "changed-uuid":
				incus.Reconcile.Instance.Config["volatile.uuid"] = "replacement"
			case "changed-config":
				writeConfigCommandFile(t, path, "SSH_PORT=2241\n")
			case "changed-host-id":
				testkit.WriteFile(t, configsync.HostIDPath(configHome), []byte("other-owner\n"), 0600)
			case "replaced-registration":
				data, _ := os.ReadFile(path)
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, path, data, 0600)
			case "changed-resource":
				incus.TeardownResources[0].Binding = "replaced"
			case "changed-project":
				incus.Reconcile.ProjectConfig["restricted.devices.gpu"] = "allow"
			case "changed-epoch":
				if err := uninstall.AdvanceOwnerEpoch(loaded.Context.Paths.DataHome, 0, 1); err != nil {
					t.Fatal(err)
				}
			case "changed-record":
				writeProfileTestFile(t, legacyContinuationPath(loaded.Context), []byte("competing"), 0600)
			}
			result := program.runPreparedCommand(context.Background(), prepared, scenario != "decline")
			snapshot, readErr := readLegacyContinuation(loaded.Context)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "accept" {
				if result != 0 || !snapshot.Exists {
					t.Fatalf("explicit typed consent failed: %d %s", result, output.String())
				}
			} else if result == 0 {
				t.Fatal("declined/stale plan executed", scenario)
			} else if snapshot.Exists && scenario != "changed-record" {
				t.Fatal("declined/stale plan published grant")
			}
			if len(incus.ConfigUpdates) != 0 || incus.Reconcile.ProjectConfig["user.subyard.installation"] != "" {
				t.Fatal("assessment or consent granted destructive project ownership")
			}
		})
	}
}

func TestLegacyContinuationRecordRejectsLinks(t *testing.T) {
	for _, kind := range []string{"symbolic", "hard"} {
		t.Run(kind, func(t *testing.T) {
			runtime, _ := legacyContinuationFixture(t)
			path := legacyContinuationPath(runtime.Yard)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(runtime.Yard.Paths.ConfigHome, "target.json")
			testkit.WriteFile(t, target, []byte("protected"), 0600)
			var err error
			if kind == "symbolic" {
				err = os.Symlink(target, path)
			} else {
				err = os.Link(target, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readLegacyContinuation(runtime.Yard); err == nil {
				t.Fatal("linked grant admitted")
			}
			data, _ := os.ReadFile(target)
			if string(data) != "protected" {
				t.Fatal("unsafe read mutated target")
			}
		})
	}
}

func TestLegacyContinuationOversizedGrantDoesNotPublish(t *testing.T) {
	runtime, _ := legacyContinuationFixture(t)
	continuation, err := assessLegacyContinuation(context.Background(), runtime, "host", "/runtime", "sealed", config.PersistentFileSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	continuation.grant.Provenance = strings.Repeat("x", 16384)
	if err := continuation.publish(&initExecution{loaded: config.Loaded{Context: runtime.Yard}}); err == nil {
		t.Fatal("oversized grant published")
	}
	snapshot, err := readLegacyContinuation(runtime.Yard)
	if err != nil || snapshot.Exists {
		t.Fatal("size refusal wrote target", err)
	}
}

func legacyContinuationInstalledFixture(t *testing.T, implicitDefault ...bool) (*CLI, *initExecution, *testkit.Incus, string) {
	t.Helper()
	program, _, configHome := completedSelectionAuthoringFixture(t)
	options := program.options
	options.Environment = environmentList(program.configSourceEnvironment(), nil)
	var setupErr error
	program, setupErr = New(options)
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	program.options.Stdin = strings.NewReader("no\n")
	program.promptInputTerminal = func() bool { return true }
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	path, err := configScalarAuthoringPath(loaded, config.ScopeYard)
	if err != nil {
		t.Fatal(err)
	}
	if len(implicitDefault) != 0 && implicitDefault[0] {
		// Published v0.8 default init consumes host config and does not author a
		// per-yard registration. Match the owning physical predecessor producer.
		writeConfigCommandFile(t, filepath.Join(configHome, "config.env"), "AGENTS=none\nBASE_IMAGE=subyard-e2e-debian-13-cloud-container\nBASE_IMAGE_FALLBACK=subyard-e2e-debian-13-cloud-container\nDEV_UID=2000\n")
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("implicit old default unexpectedly has a registration", err)
		}
	} else if _, err := os.Stat(path); os.IsNotExist(err) {
		writeConfigCommandFile(t, path, "SSH_PORT=2240\n")
	}
	program.env = environmentMap(program.options.Environment)
	loaded, err = program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := program.env["YARD_RUNTIME_ROOT"]
	installation := uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()), DataHome: loaded.Context.Paths.DataHome, ConfigHome: configHome, RuntimeRoot: runtimeRoot, CacheRoot: filepath.Join(loaded.Context.Paths.DataHome, "releases"), BinDir: filepath.Join(loaded.Context.Paths.OperatorHome, ".local/bin")}
	payload, _ := json.Marshal(installation)
	writeProfileTestFile(t, filepath.Join(runtimeRoot, "installation.json"), payload, 0600)
	hostID, err := configsync.EnsureHostID(configHome, loaded.Environment)
	if err != nil {
		t.Fatal(err)
	}
	runtime, incus := legacyContinuationFixture(t)
	runtime.Yard = loaded.Context
	runtime.RepositoryRoot = program.options.RepositoryRoot
	runtime.Environment = environmentList(loaded.Environment, nil)
	instance := incus.Reconcile.Instance
	instance.Name = loaded.Context.YardInstanceName
	instance.Project = loaded.Context.IncusProject
	instance.Type = loaded.Context.YardKind
	instance.Config["user.subyard.name"] = loaded.Context.YardName
	instance.Config["user.subyard.bridge"] = loaded.Context.IncusBridge
	incus.Reconcile.Instance = instance
	pool, volume := loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"]
	if pool == "" {
		pool = "default"
	}
	if volume == "" {
		volume = "yard-srv"
	}
	runtime.SRVPool = pool
	runtime.SRVVolume = volume
	incus.Reconcile.ProfileDevices["root"]["pool"] = pool
	incus.Reconcile.ProfileDevices["eth0"]["network"] = loaded.Context.IncusBridge
	incus.TeardownResources = []ports.TeardownResource{{Kind: "project", Name: loaded.Context.IncusProject, Binding: "project"}, {Kind: "profile", Name: "default", Binding: "profile"}, {Kind: "instance", Name: instance.Name, Binding: "instance"}, {Kind: "volume", Name: volume, Pool: pool, Binding: "volume"}}
	canonicalLegacyInstanceFixture(runtime, incus)
	execution := &initExecution{loaded: loaded, platform: runtime, hostID: hostID}
	return program, execution, incus, path
}

func TestLegacyContinuationRejectsUnknownInitialPermission(t *testing.T) {
	for _, kind := range []string{"GPU", "profile-raw", "profile-security", "network-drift", "escape-disk", "instance-security", "instance-raw", "unknown-proxy", "nic-override", "instance-environment", "device-permission", "idmap-privileged", "idmap-duplicate", "idmap-unknown", "idmap-overflow", "idmap-negative", "idmap-empty", "fake-apparmor", "fake-preview", "unverified-kvm"} {
		t.Run(kind, func(t *testing.T) {
			program, execution, incus, _ := legacyContinuationInstalledFixture(t)
			switch kind {
			case "GPU":
				incus.Reconcile.ProjectConfig["restricted.devices.gpu"] = "allow"
			case "profile-raw":
				incus.Reconcile.ProfileConfig = map[string]string{"raw.lxc": "lxc.apparmor.profile=unconfined"}
			case "profile-security":
				incus.Reconcile.ProfileConfig = map[string]string{"security.privileged": "true"}
			case "escape-disk":
				incus.Reconcile.Instance.LocalDevices["escape"] = map[string]string{"type": "disk", "source": "/etc", "path": "/host-etc", "readonly": "true"}
				incus.Reconcile.Instance.Devices["escape"] = incus.Reconcile.Instance.LocalDevices["escape"]
			case "instance-security":
				incus.Reconcile.Instance.LocalConfig["security.privileged"] = "true"
				incus.Reconcile.Instance.Config["security.privileged"] = "true"
			case "instance-raw":
				incus.Reconcile.Instance.LocalConfig["raw.lxc"] = "lxc.apparmor.profile=unconfined"
				incus.Reconcile.Instance.Config["raw.lxc"] = incus.Reconcile.Instance.LocalConfig["raw.lxc"]
			case "idmap-privileged", "idmap-duplicate", "idmap-unknown", "idmap-overflow", "idmap-negative", "idmap-empty":
				value := incus.Reconcile.Instance.Config["volatile.idmap.current"]
				switch kind {
				case "idmap-privileged":
					value = strings.ReplaceAll(value, `"Hostid":1000000`, `"Hostid":0`)
				case "idmap-duplicate":
					value = strings.ReplaceAll(value, `"Isuid":false,"Isgid":true`, `"Isuid":true,"Isgid":false`)
				case "idmap-unknown":
					value = strings.Replace(value, `"Nsid":0`, `"UnknownPermission":true,"Nsid":0`, 1)
				case "idmap-overflow":
					value = strings.ReplaceAll(value, `"Hostid":1000000`, `"Hostid":4294967295`)
				case "idmap-negative":
					value = strings.ReplaceAll(value, `"Nsid":0`, `"Nsid":-1`)
				case "idmap-empty":
					value = `[]`
				}
				for _, values := range []map[string]string{incus.Reconcile.Instance.Config, incus.Reconcile.Instance.LocalConfig} {
					values["volatile.idmap.current"] = value
					values["volatile.idmap.next"] = value
				}
			case "fake-apparmor":
				device := map[string]string{"type": "disk", "source": "/etc", "path": "/sys/module/apparmor/parameters/enabled", "readonly": "true"}
				incus.Reconcile.Instance.LocalDevices["subyard-docker-apparmor"] = device
				incus.Reconcile.Instance.Devices["subyard-docker-apparmor"] = device
			case "fake-preview":
				device := previewroute.Device("10.80.0.1", "12345")
				device["connect"] = "tcp:127.0.0.1:22"
				incus.Reconcile.Instance.LocalDevices[previewroute.DeviceName] = device
				incus.Reconcile.Instance.Devices[previewroute.DeviceName] = device
			case "unverified-kvm":
				device := map[string]string{"type": "unix-char", "source": "/dev/kvm", "path": "/dev/kvm", "mode": "0660", "gid": "1000"}
				incus.Reconcile.Instance.LocalDevices["kvm"] = device
				incus.Reconcile.Instance.Devices["kvm"] = device
			case "nic-override":
				incus.Reconcile.Instance.LocalDevices["eth0"] = map[string]string{"type": "nic", "network": "foreign"}
				incus.Reconcile.Instance.Devices["eth0"] = incus.Reconcile.Instance.LocalDevices["eth0"]
			case "instance-environment":
				incus.Reconcile.Instance.Config["environment.LD_PRELOAD"] = "/escape"
				incus.Reconcile.Instance.LocalConfig["environment.LD_PRELOAD"] = "/escape"
			case "device-permission":
				incus.Reconcile.Instance.LocalDevices["srv"]["readonly"] = "false"
				incus.Reconcile.Instance.Devices["srv"]["readonly"] = "false"
			case "unknown-proxy":
				incus.Reconcile.Instance.LocalDevices["escape"] = map[string]string{"type": "proxy", "listen": "tcp:0.0.0.0:2222", "connect": "tcp:127.0.0.1:22"}
				incus.Reconcile.Instance.Devices["escape"] = incus.Reconcile.Instance.LocalDevices["escape"]
			case "network-drift":
				native := execution.platform.(reconcileruntime.Runtime)
				native.NetworkPolicy = legacyContinuationNetworkFixture{physical: true}
				execution.platform = native
			}
			if kind == "GPU" && incus.Reconcile.ProjectConfig["restricted.devices.gpu"] != "allow" {
				t.Fatal("GPU precondition missing")
			}
			if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{continueLegacy: true}); err == nil {
				t.Fatal("unsafe initial permission admitted", kind)
			}
			snapshot, err := readLegacyContinuation(execution.loaded.Context)
			if err != nil || snapshot.Exists || len(incus.ConfigUpdates) != 0 {
				t.Fatal("initial refusal wrote state", err)
			}
		})
	}
}

func TestLegacyContinuationFreshSessionPolicyBinding(t *testing.T) {
	for _, change := range []string{"GPU", "project-feature", "profile-device", "profile-nic-security", "profile-config", "escape-disk", "instance-security", "instance-raw", "device-source", "device-permission", "nic-override", "unknown-config", "idmap-change", "runtime-only", "managed-apparmor", "managed-preview", "publication-only", "canonical-kvm"} {
		t.Run(change, func(t *testing.T) {
			program, execution, incus, _ := legacyContinuationInstalledFixture(t)
			if change == "canonical-kvm" {
				device := map[string]string{"type": "unix-char", "source": "/dev/kvm", "path": "/dev/kvm", "mode": "0660", "gid": "108"}
				incus.Reconcile.Instance.LocalDevices["kvm"] = maps.Clone(device)
				incus.Reconcile.Instance.Devices["kvm"] = maps.Clone(device)
				incus.ExecSteps = []testkit.IncusExecStep{{Result: ports.InstanceExecResult{Stdout: []byte("kvm:x:108:\n")}}}
				native := execution.platform.(reconcileruntime.Runtime)
				native.Executor = incus
				execution.platform = native
			}
			if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{continueLegacy: true}); err != nil {
				t.Fatal(err)
			}
			if err := execution.legacyContinuation.publish(execution); err != nil {
				t.Fatal(err)
			}
			before, err := readLegacyContinuation(execution.loaded.Context)
			if err != nil || !before.Exists {
				t.Fatal("real grant establishment failed", err)
			}
			if incus.Reconcile.Instance.LocalDevices == nil {
				incus.Reconcile.Instance.LocalDevices = map[string]map[string]string{}
			}
			if incus.Reconcile.Instance.Devices == nil {
				incus.Reconcile.Instance.Devices = map[string]map[string]string{}
			}
			if incus.Reconcile.Instance.LocalConfig == nil {
				incus.Reconcile.Instance.LocalConfig = map[string]string{}
			}
			switch change {
			case "GPU":
				incus.Reconcile.ProjectConfig["restricted.devices.gpu"] = "allow"
			case "project-feature":
				incus.Reconcile.ProjectConfig["features.images"] = "true"
			case "profile-nic-security":
				incus.Reconcile.ProfileDevices["eth0"]["security.ipv4_filtering"] = "false"
			case "profile-device":
				incus.Reconcile.ProfileDevices["root"]["readonly"] = "true"
			case "profile-config":
				incus.Reconcile.ProfileConfig = map[string]string{"raw.lxc": "lxc.apparmor.profile=unconfined"}
			case "escape-disk":
				incus.Reconcile.Instance.LocalDevices["escape"] = map[string]string{"type": "disk", "source": "/etc", "path": "/host-etc", "readonly": "true"}
				incus.Reconcile.Instance.Devices["escape"] = incus.Reconcile.Instance.LocalDevices["escape"]
			case "instance-security":
				incus.Reconcile.Instance.LocalConfig["security.privileged"] = "true"
				incus.Reconcile.Instance.Config["security.privileged"] = "true"
			case "instance-raw":
				incus.Reconcile.Instance.LocalConfig["raw.lxc"] = "lxc.apparmor.profile=unconfined"
				incus.Reconcile.Instance.Config["raw.lxc"] = incus.Reconcile.Instance.LocalConfig["raw.lxc"]
			case "device-permission":
				incus.Reconcile.Instance.LocalDevices["srv"]["readonly"] = "false"
				incus.Reconcile.Instance.Devices["srv"]["readonly"] = "false"
			case "nic-override":
				incus.Reconcile.Instance.LocalDevices["eth0"] = maps.Clone(incus.Reconcile.Instance.Devices["eth0"])
			case "unknown-config":
				incus.Reconcile.Instance.Config["user.subyard.unapproved"] = "true"
				incus.Reconcile.Instance.LocalConfig["user.subyard.unapproved"] = "true"
			case "idmap-change":
				incus.Reconcile.Instance.Config["volatile.idmap.next"] = "[]"
				incus.Reconcile.Instance.LocalConfig["volatile.idmap.next"] = "[]"
			case "managed-apparmor":
				device := map[string]string{"type": "disk", "source": "/dev/null", "path": "/sys/module/apparmor/parameters/enabled", "readonly": "true"}
				incus.Reconcile.Instance.LocalDevices["subyard-docker-apparmor"] = maps.Clone(device)
				incus.Reconcile.Instance.Devices["subyard-docker-apparmor"] = maps.Clone(device)
			case "managed-preview":
				port := execution.loaded.Environment["WEB_PREVIEW_HOST_PORT"]
				if port == "" {
					t.Fatal("missing real derived preview port")
				}
				marker := "v1:10.80.0.1:" + port
				receipt, err := json.Marshal(ownerpublication.Receipt{Version: 1, Ownership: marker, Device: previewroute.Device("10.80.0.1", port)})
				if err != nil {
					t.Fatal(err)
				}
				for _, values := range []map[string]string{incus.Reconcile.Instance.Config, incus.Reconcile.Instance.LocalConfig} {
					values[previewroute.Key] = marker
					values[ownerpublication.Prefix+previewroute.DeviceName] = string(receipt)
				}
			case "publication-only":
				for _, values := range []map[string]string{incus.Reconcile.Instance.Config, incus.Reconcile.Instance.LocalConfig} {
					values["user.subyard.preview_sha256"] = strings.Repeat("a", 64)
					values["user.subyard.preview_endpoint_sha256"] = strings.Repeat("b", 64)
					values["user.subyard.ccusage_version"] = "21.0.0"
				}
			case "device-source":
				incus.Reconcile.Instance.LocalDevices["srv"] = map[string]string{"type": "disk", "source": "/etc", "path": "/srv"}
				incus.Reconcile.Instance.Devices["srv"] = incus.Reconcile.Instance.LocalDevices["srv"]
			case "runtime-only", "canonical-kvm":
				incus.Reconcile.Instance.Status = "stopped"
				incus.Reconcile.Instance.Config["user.subyard.desired_power"] = "stopped"
				incus.Reconcile.Instance.LocalConfig["user.subyard.desired_power"] = "stopped"
				incus.Reconcile.Instance.Config["volatile.last_state.power"] = "STOPPED"
				incus.Reconcile.Instance.LocalConfig["volatile.last_state.power"] = "STOPPED"
				for _, values := range []map[string]string{incus.Reconcile.Instance.Config, incus.Reconcile.Instance.LocalConfig} {
					values["volatile.last_state.ready"] = "false"
					values["volatile.eth0.host_name"] = "veth-new"
					values["volatile.eth0.last_state.ip_addresses"] = "10.80.0.2"
				}
			}
			nativeApply := execution.platform.(reconcileruntime.Runtime)
			nativeErr := nativeApply.ApplyStage(context.Background(), ports.ReconcileStageProject)
			if change == "runtime-only" || change == "managed-apparmor" || change == "managed-preview" || change == "publication-only" || change == "canonical-kvm" {
				if nativeErr != nil {
					t.Fatal("runtime guard rejected unchanged policy", nativeErr)
				}
			} else if !errors.Is(nativeErr, domain.ErrPlanStale) {
				t.Error("runtime guard accepted changed policy", change, nativeErr)
			}
			for _, optIn := range []bool{false, true} {
				fresh, err := New(program.options)
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := fresh.loadContext("default")
				if err != nil {
					t.Fatal(err)
				}
				native := execution.platform.(reconcileruntime.Runtime)
				native.LegacyContinuation = nil
				native.Yard = loaded.Context
				request := &initExecution{loaded: loaded, platform: native, hostID: execution.hostID}
				err = fresh.prepareLegacyContinuation(context.Background(), request, initArguments{continueLegacy: optIn})
				if change == "runtime-only" || change == "managed-apparmor" || change == "managed-preview" || change == "publication-only" || change == "canonical-kvm" {
					if err != nil {
						t.Fatal("ephemeral runtime state invalidated stable authority", err)
					}
				} else if err == nil {
					t.Error("fresh session accepted changed approved policy", change, optIn)
				}
				current, readErr := readLegacyContinuation(loaded.Context)
				if readErr != nil || !reflect.DeepEqual(current, before) || len(incus.ConfigUpdates) != 0 {
					t.Fatal("fresh-session assessment rewrote grant/native state", readErr)
				}
			}
			if change == "canonical-kvm" && (len(incus.ExecCalls) != 1 || !reflect.DeepEqual(incus.ExecCalls[0].Request.Command, []string{"getent", "group", "kvm"})) {
				t.Fatal("KVM assessment did not retain its one read-only guest observation", incus.ExecCalls)
			}
		})
	}
}

func TestLegacyContinuationMissingConfigHomeAssessment(t *testing.T) {
	for _, scenario := range []string{"plain", "explicit", "unsafe-link", "unsafe-directory"} {
		t.Run(scenario, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			configHome := filepath.Join(testkit.TempDir(t), "missing-config")
			environment = append(environment, "SUBYARD_CONFIG_HOME="+configHome)
			program, err := New(Options{RepositoryRoot: root, Environment: environment})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(configHome); !os.IsNotExist(err) {
				t.Fatal("absent config root precondition missing", err)
			}
			if scenario == "unsafe-link" {
				if err := os.Symlink(filepath.Join(configHome, "absent"), configHome); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unsafe-directory" {
				if err := os.Mkdir(configHome, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(configHome, 0777); err != nil {
					t.Fatal(err)
				}
			}
			runtime := program.initPlatform(loaded, nil).(reconcileruntime.Runtime)
			runtime.Incus = &testkit.Incus{}
			execution := &initExecution{loaded: loaded, platform: runtime}
			err = program.prepareLegacyContinuation(context.Background(), execution, initArguments{continueLegacy: scenario == "explicit"})
			if scenario == "plain" {
				if err != nil {
					t.Fatal("ordinary fresh native assessment rejected missing config root", err)
				}
			} else if err == nil {
				t.Fatal("explicit/unsafe missing provenance accepted")
			}
			info, statErr := os.Lstat(configHome)
			if scenario == "unsafe-link" {
				if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("assessment changed unsafe link")
				}
			} else if scenario == "unsafe-directory" {
				if statErr != nil || info.Mode().Perm() != 0777 {
					t.Fatal("unsafe directory metadata changed", statErr)
				}
				entries, err := os.ReadDir(configHome)
				if err != nil || len(entries) != 0 {
					t.Fatal("unsafe root assessment wrote files", err)
				}
			} else if !os.IsNotExist(statErr) {
				t.Fatal("read-only assessment created config root", statErr)
			}
		})
	}
}

type legacyContinuationNetworkFixture struct{ physical bool }

func (legacyContinuationNetworkFixture) Check(context.Context, yardnetwork.Yard) error { return nil }
func (legacyContinuationNetworkFixture) Ensure(context.Context, yardnetwork.Yard) error {
	panic("legacy assessment must not write native network policy")
}
func (legacyContinuationNetworkFixture) WithStart(_ context.Context, _ yardnetwork.Yard, start func() error) error {
	return start()
}
func (fixture legacyContinuationNetworkFixture) Prepare(_ context.Context, yards []yardnetwork.Yard, _ yardnetwork.Change) (yardnetwork.Plan, error) {
	plan := yardnetwork.Plan{Physical: fixture.physical}
	if fixture.physical {
		plan.Updates = []yardnetwork.Update{{Yard: yardnetwork.ObservedYard{Yard: yards[0]}}}
	}
	return plan, nil
}

func TestLegacyContinuationImplicitDefaultRegistration(t *testing.T) {
	for _, scenario := range []string{"fresh-reuse", "host-stale", "host-inode", "yard-created", "missing-host", "unsafe-host-mode", "unsafe-host-link", "unsafe-yard-mode", "unsafe-yard-link", "named-missing"} {
		t.Run(scenario, func(t *testing.T) {
			program, execution, incus, yardPath := legacyContinuationInstalledFixture(t, true)
			home := execution.loaded.Context.Paths.ConfigHome
			hostPath := filepath.Join(home, "config.env")
			hostBefore, err := config.ReadPersistentFileSnapshot(home, hostPath)
			if err != nil || !hostBefore.Exists || hostBefore.Identity.Mode&0777 != 0600 {
				t.Fatal("old producer protected host precondition", err)
			}
			yardBefore, err := config.ReadPersistentFileSnapshot(home, yardPath)
			if err != nil || yardBefore.Exists {
				t.Fatal("old producer absent yard precondition", err)
			}
			switch scenario {
			case "missing-host":
				if err := os.Remove(hostPath); err != nil {
					t.Fatal(err)
				}
			case "unsafe-host-mode":
				if err := os.Chmod(hostPath, 0644); err != nil {
					t.Fatal(err)
				}
			case "unsafe-host-link":
				if err := os.Remove(hostPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("absent-target", hostPath); err != nil {
					t.Fatal(err)
				}
			case "unsafe-yard-mode":
				if err := os.MkdirAll(filepath.Dir(yardPath), 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, yardPath, []byte("SSH_PORT=2222\n"), 0644)
			case "unsafe-yard-link":
				if err := os.MkdirAll(filepath.Dir(yardPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(hostPath, yardPath); err != nil {
					t.Fatal(err)
				}
			case "named-missing":
				execution.loaded.Context.YardName = "other"
				native := execution.platform.(reconcileruntime.Runtime)
				native.Yard = execution.loaded.Context
				execution.platform = native
			}
			err = program.prepareLegacyContinuation(context.Background(), execution, initArguments{continueLegacy: true})
			unsafe := strings.HasPrefix(scenario, "unsafe-") || scenario == "missing-host" || scenario == "named-missing"
			if unsafe {
				if err == nil {
					t.Fatal("unsafe or non-default source authorized continuation", scenario)
				}
			} else {
				if err != nil {
					t.Fatal("genuine implicit default producer refused", err)
				}
				switch scenario {
				case "host-stale":
					testkit.WriteFile(t, hostPath, append(hostBefore.Content, []byte("# changed after consent\n")...), 0600)
				case "host-inode":
					// Keep the original inode alive so replacement cannot reuse it.
					retained := hostPath + ".before-inode"
					if err := os.Rename(hostPath, retained); err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, hostPath, hostBefore.Content, 0600)
					original, err := os.Stat(retained)
					if err != nil {
						t.Fatal(err)
					}
					replacement, err := os.Stat(hostPath)
					if err != nil || os.SameFile(original, replacement) {
						t.Fatal("replacement inode precondition not established", err)
					}
				case "yard-created":
					writeConfigCommandFile(t, yardPath, "SSH_PORT=2222\n")
				}
				err = execution.legacyContinuation.check(context.Background(), program, execution)
				if scenario != "fresh-reuse" {
					if !errors.Is(err, domain.ErrPlanStale) {
						t.Fatal("changed protected old input admitted", scenario, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if err := execution.legacyContinuation.publish(execution); err != nil {
						t.Fatal(err)
					}
					grantBefore, err := readLegacyContinuation(execution.loaded.Context)
					if err != nil || !grantBefore.Exists {
						t.Fatal("grant absent", err)
					}
					if err := execution.platform.(reconcileruntime.Runtime).ApplyStage(context.Background(), ports.ReconcileStageProject); err != nil {
						t.Fatal("native reuse guard", err)
					}
					for _, optIn := range []bool{false, true} {
						fresh, err := New(program.options)
						if err != nil {
							t.Fatal(err)
						}
						loaded, err := fresh.loadContext("default")
						if err != nil {
							t.Fatal(err)
						}
						native := execution.platform.(reconcileruntime.Runtime)
						native.LegacyContinuation = nil
						native.Yard = loaded.Context
						request := &initExecution{loaded: loaded, platform: native, hostID: execution.hostID}
						if err := fresh.prepareLegacyContinuation(context.Background(), request, initArguments{continueLegacy: optIn}); err != nil {
							t.Fatal("fresh implicit default reuse", optIn, err)
						}
						after, err := readLegacyContinuation(loaded.Context)
						if err != nil || !reflect.DeepEqual(after, grantBefore) {
							t.Fatal("reuse rewrote grant", err)
						}
					}
					hostAfter, err := config.ReadPersistentFileSnapshot(home, hostPath)
					if err != nil || !reflect.DeepEqual(hostBefore, hostAfter) {
						t.Fatal("continuation rewrote old host config", err)
					}
					yardAfter, err := config.ReadPersistentFileSnapshot(home, yardPath)
					if err != nil || yardAfter.Exists {
						t.Fatal("continuation fabricated yard registration", err)
					}
				}
			}
			grant, readErr := readLegacyContinuation(execution.loaded.Context)
			if readErr != nil || (scenario != "fresh-reuse" && grant.Exists) || len(incus.ConfigUpdates) != 0 || incus.Reconcile.ProjectConfig["user.subyard.installation"] != "" {
				t.Fatal("refusal/reuse changed native or grant authority", readErr)
			}
		})
	}
}

// The core contract uses synthetic ownership, not a shipped profile exception.
func TestLegacyContinuationDeclaredPublication(t *testing.T) {
	for _, scenario := range []string{"old-to-current", "current", "initial-partial", "initial-malformed", "initial-enabled", "initial-foreign-source", "own-source", "changed-source", "changed-setting", "changed-protected-setting", "partial", "schema", "enabled", "malformed", "foreign-key", "raw-recheck"} {
		t.Run(scenario, func(t *testing.T) {
			program, execution, incus, _ := legacyContinuationInstalledFixture(t, true)
			tail := "one"
			native := execution.platform.(reconcileruntime.Runtime)
			native.Profiles = []profile.Definition{{Name: "synthetic", InstancePublication: &profile.RevisionedSettingsPublication{SchemaKey: "user.subyard.synthetic_schema", SchemaValue: "1", RevisionKey: "user.subyard.synthetic_revision", DisabledSetting: "SYN_ENABLED", LegacySettingsCount: 1, Settings: []profile.PublicationSetting{{Name: "SYN_IMAGE"}, {Name: "SYN_LIMIT", LegacyDefault: &tail}}}}}
			native.Environment = append(native.Environment, "SYN_ENABLED=0", "SYN_IMAGE=images:debian/13/cloud", "SYN_LIMIT=one")
			execution.platform = native
			old := "0:" + strings.Repeat("a", 64) + ":images:debian/13/cloud"
			modern := "0:" + strings.Repeat("b", 64) + ":images:debian/13/cloud:one"
			if scenario == "initial-foreign-source" || scenario == "own-source" || scenario == "changed-source" {
				source := native.Yard.YardName
				if scenario == "initial-foreign-source" {
					source = "foreign"
				}
				native.Profiles[0].InstancePublication.Settings[0] = profile.PublicationSetting{Name: "SYN_SOURCE", FallbackSetting: "YARD_NAME"}
				native.Environment = append(native.Environment, "SYN_SOURCE="+source, "YARD_NAME="+source)
				execution.platform = native
				old = "0:" + strings.Repeat("a", 64) + ":" + source
				modern = "0:" + strings.Repeat("b", 64) + ":" + source + ":one"
			}
			for _, values := range []map[string]string{incus.Reconcile.Instance.Config, incus.Reconcile.Instance.LocalConfig} {
				values["user.subyard.synthetic_schema"] = "1"
				values["user.subyard.synthetic_revision"] = old
				if scenario == "current" {
					values["user.subyard.synthetic_revision"] = modern
				}
			}
			if strings.HasPrefix(scenario, "initial-") {
				for _, values := range []map[string]string{incus.Reconcile.Instance.Config, incus.Reconcile.Instance.LocalConfig} {
					switch scenario {
					case "initial-partial":
						delete(values, "user.subyard.synthetic_schema")
					case "initial-malformed":
						values["user.subyard.synthetic_revision"] = "0:invalid:images:debian/13/cloud"
					case "initial-enabled":
						values["user.subyard.synthetic_revision"] = "1" + old[1:]
					}
				}
				if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{continueLegacy: true}); err == nil {
					t.Fatal("invalid initial metadata admitted", scenario)
				}
				grant, err := readLegacyContinuation(execution.loaded.Context)
				if err != nil || grant.Exists || len(incus.ConfigUpdates) != 0 {
					t.Fatal("invalid initial publication changed authority", err)
				}
				return
			}
			if err := program.prepareLegacyContinuation(context.Background(), execution, initArguments{continueLegacy: true}); err != nil {
				t.Fatal("valid declared publication refused", err)
			}
			if scenario == "raw-recheck" {
				for _, values := range []map[string]string{incus.Reconcile.Instance.Config, incus.Reconcile.Instance.LocalConfig} {
					values["user.subyard.synthetic_revision"] = modern
				}
				if err := execution.legacyContinuation.check(context.Background(), program, execution); !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("raw exact consent observation was projected away", err)
				}
				grant, err := readLegacyContinuation(execution.loaded.Context)
				if err != nil || grant.Exists {
					t.Fatal("stale raw observation published grant", err)
				}
				return
			}
			if err := execution.legacyContinuation.publish(execution); err != nil {
				t.Fatal(err)
			}
			before, err := readLegacyContinuation(execution.loaded.Context)
			if err != nil || !before.Exists {
				t.Fatal(err)
			}
			for _, values := range []map[string]string{incus.Reconcile.Instance.Config, incus.Reconcile.Instance.LocalConfig} {
				switch scenario {
				case "old-to-current":
					values["user.subyard.synthetic_revision"] = modern
				case "changed-setting", "changed-protected-setting":
					values["user.subyard.synthetic_revision"] = "0:" + strings.Repeat("b", 64) + ":images:debian/13/cloud:two"
				case "partial":
					delete(values, "user.subyard.synthetic_schema")
				case "schema":
					values["user.subyard.synthetic_schema"] = "2"
				case "enabled":
					values["user.subyard.synthetic_revision"] = "1:" + strings.Repeat("b", 64) + ":images:debian/13/cloud:one"
				case "malformed":
					values["user.subyard.synthetic_revision"] = strings.ToUpper(modern)
				case "foreign-key":
					values["user.subyard.synthetic_foreign"] = "true"
				}
			}
			if scenario == "changed-source" {
				changed := execution.platform.(reconcileruntime.Runtime)
				changed.Environment = append(changed.Environment, "SYN_SOURCE=foreign", "YARD_NAME=foreign")
				execution.platform = changed
				for _, values := range []map[string]string{incus.Reconcile.Instance.Config, incus.Reconcile.Instance.LocalConfig} {
					values["user.subyard.synthetic_revision"] = "0:" + strings.Repeat("b", 64) + ":foreign:one"
				}
			}
			if scenario == "changed-protected-setting" {
				changed := execution.platform.(reconcileruntime.Runtime)
				for i, value := range changed.Environment {
					if value == "SYN_LIMIT=one" {
						changed.Environment[i] = "SYN_LIMIT=two"
					}
				}
				execution.platform = changed
			}
			valid := scenario == "old-to-current" || scenario == "current" || scenario == "own-source"
			err = execution.platform.(reconcileruntime.Runtime).ApplyStage(context.Background(), ports.ReconcileStageProject)
			if valid && err != nil || !valid && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("runtime publication policy result", scenario, err)
			}
			for _, optIn := range []bool{false, true} {
				fresh, err := New(program.options)
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := fresh.loadContext("default")
				if err != nil {
					t.Fatal(err)
				}
				runtime := execution.platform.(reconcileruntime.Runtime)
				runtime.LegacyContinuation = nil
				request := &initExecution{loaded: loaded, platform: runtime, hostID: execution.hostID}
				err = fresh.prepareLegacyContinuation(context.Background(), request, initArguments{continueLegacy: optIn})
				if valid && err != nil || !valid && err == nil {
					t.Fatal("fresh publication binding result", scenario, optIn, err)
				}
			}
			after, err := readLegacyContinuation(execution.loaded.Context)
			if err != nil || !reflect.DeepEqual(before, after) || len(incus.ConfigUpdates) != 0 {
				t.Fatal("publication check changed grant/native state", err)
			}
		})
	}
}
