package releasetransition_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	rt "github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

const (
	crashFamilyRegistered = "registered"
	// A yard whose test VM revision is rewritten by activation after every row is Verified.
	crashFamilyRegisteredRevision = crashFamilyRegistered + "-test-vms-revision"
	// A VM yard, whose guest restart leaves the Incus lifecycle outputs seen on real hosts.
	crashFamilyRegisteredVM = crashFamilyRegistered + "-vm"
)

const testVMsRevisionKey = "user.subyard.test_vms_revision"

const registeredCrashRegistry = `{"schemaVersion":2,"minimumEpochs":{"owner-registration":2,"power-metadata":2,"settings":3,"project-state":1},"currentEpochs":{"owner-registration":3,"power-metadata":3,"settings":3,"project-state":1},"migrations":[{"id":"registered-v3","domain":"owner-registration","fromEpoch":2,"toEpoch":3,"kind":"registered-yards-v2-to-v3"},{"id":"guest-tmp-v3","domain":"power-metadata","fromEpoch":2,"toEpoch":3,"kind":"registered-guest-disk-tmp-v2-to-v3","dependsOn":["registered-v3"]}]}`

const registeredCrashProfile = `{"schema_version":1,"settings":[{"name":"SAMPLE_HOST","type":"string","scopes":["yard","shipped"],"application":"next-command","syncable":true},{"name":"SAMPLE_PORT","type":"port","minimum":1,"maximum":65535,"scopes":["yard","shipped"],"application":"next-command","syncable":true}]}`

// registeredCrashNative keeps native Incus and guest state across process kills.
type registeredCrashNative struct {
	*testkit.Incus
	path  string
	trace *crashTrace
	vm    bool
}

type registeredCrashState struct {
	Instance ports.InstanceInfo
	Guest    string
}

func (native *registeredCrashNative) read() (registeredCrashState, error) {
	var state registeredCrashState
	body, err := os.ReadFile(native.path)
	if err == nil {
		err = json.Unmarshal(body, &state)
	}
	return state, err
}

func (native *registeredCrashNative) write(state registeredCrashState) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := crashWrite(native.path, body); err != nil {
		return err
	}
	native.load(state)
	return nil
}

func (native *registeredCrashNative) load(state registeredCrashState) {
	native.Instances["fixture/yard"] = state.Instance
	native.Reconcile.Instance = state.Instance
}

func (native *registeredCrashNative) AdoptRegisteredInstance(_ context.Context, _ ports.InstanceInfo, values map[string]string) error {
	state, err := native.read()
	if err != nil {
		return err
	}
	for key, value := range values {
		state.Instance.Config[key] = value
		state.Instance.LocalConfig[key] = value
	}
	if err := native.write(state); err != nil {
		return err
	}
	if values["user.subyard.disk_tmp"] == "v1" {
		return native.trace.hit("native/guest-tmp/marker")
	}
	return native.trace.hit("native/registered/receipts")
}

func (native *registeredCrashNative) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	action := request.Command[len(request.Command)-1]
	if action != "apply" {
		return ports.InstanceExecResult{}, nil
	}
	state, err := native.read()
	if err != nil {
		return ports.InstanceExecResult{}, err
	}
	state.Guest = "policy-applied"
	if native.vm {
		for _, values := range []map[string]string{state.Instance.Config, state.Instance.LocalConfig} {
			values["volatile.eth0.host_name"], values["volatile.vm.rtc_adjustment"], values["volatile.vm.needs_reset"] = "tapAFTER", "-2", "true"
		}
	}
	if err := native.write(state); err != nil {
		return ports.InstanceExecResult{}, err
	}
	return ports.InstanceExecResult{}, native.trace.hit("native/guest-tmp/apply")
}

// registeredCrashConfig adds the sample endpoint profile to the shipped catalog.
func registeredCrashConfig(t *testing.T, destination string) {
	t.Helper()
	source, err := filepath.Abs("../../config")
	if err != nil {
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
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		testkit.WriteFile(t, target, body, info.Mode().Perm())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sample := filepath.Join(destination, "profiles/sample")
	if err := os.Mkdir(sample, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(sample, "profile.json"), []byte(registeredCrashProfile), 0o600)
	testkit.WriteFile(t, filepath.Join(sample, "profile.conf"), []byte("SAMPLE_HOST=localhost\nSAMPLE_PORT=31000\n"), 0o600)
}

func newRegisteredCrashFixture(t *testing.T) *crashFixture {
	t.Helper()
	return registeredCrashFixtureAt(t, testkit.TempDir(t), true, crashFamilyRegistered)
}

func registeredCrashFamily(family string) func(*testing.T) *crashFixture {
	return func(t *testing.T) *crashFixture {
		t.Helper()
		return registeredCrashFixtureAt(t, testkit.TempDir(t), true, family)
	}
}

func registeredCrashFixtureAt(t *testing.T, root string, initialize bool, family string) *crashFixture {
	t.Helper()
	home := filepath.Join(root, "config")
	runtimeRoot := filepath.Join(root, "runtime")
	candidate := filepath.Join(root, "candidate", "config")
	settings := filepath.Join(home, "yards/default/config.env")
	statePath := filepath.Join(root, "native.json")
	store, err := rt.NewPOSIXV2Store(mustMkdirAll(t, home, initialize))
	if err != nil {
		t.Fatal(err)
	}
	trace := &crashTrace{store: store, cuts: map[string]bool{}}
	native := &registeredCrashNative{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{}, Reconcile: ports.ReconcileState{ProjectFound: true, InstanceFound: true, ProjectConfig: map[string]string{"user.subyard.installation": "1", "user.subyard.operator_uid": strconv.Itoa(os.Getuid()), "user.subyard.config_home": home, "user.subyard.data_home": home + "/data"}}}, path: statePath, trace: trace, vm: family == crashFamilyRegisteredVM}
	if initialize {
		for _, path := range []string{filepath.Dir(settings), filepath.Join(home, ".sync/settings"), filepath.Join(runtimeRoot, "releases/release-a"), filepath.Join(root, "scripts")} {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		registeredCrashConfig(t, candidate)
		testkit.WriteFile(t, settings, []byte("ENVIRONMENT_PROFILES=sample\n"), 0o600)
		testkit.WriteFile(t, filepath.Join(home, ".sync/settings/config.env"), []byte("SSH_PORT=2240\n"), 0o600)
		testkit.WriteFile(t, filepath.Join(root, "scripts/guest-disk-tmp.sh"), []byte("policy source\n"), 0o600)
		if err := os.Symlink("releases/release-a", filepath.Join(runtimeRoot, "current")); err != nil {
			t.Fatal(err)
		}
		values := map[string]string{"volatile.uuid": "uuid-original", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.name": "default", "user.subyard.bridge": "fixture-br", "user.subyard.desired_power": "running"}
		devices := map[string]map[string]string{"sample-server": {"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:32000", "connect": "tcp:127.0.0.1:9119"}}
		if family == crashFamilyRegisteredRevision {
			values[testVMsRevisionKey] = "revision-before"
		}
		if native.vm {
			values["volatile.eth0.host_name"], values["volatile.vm.rtc_adjustment"] = "tapBEFORE", "-1"
		}
		instance := ports.InstanceInfo{Project: "fixture", Name: "yard", Status: "Running", Config: values, LocalConfig: maps.Clone(values), Devices: devices, LocalDevices: maps.Clone(devices)}
		if native.vm {
			instance.Type = domain.YardVM
		}
		if err := native.write(registeredCrashState{Instance: instance}); err != nil {
			t.Fatal(err)
		}
	}
	state, err := native.read()
	if err != nil {
		t.Fatal(err)
	}
	native.load(state)

	declaration, err := profile.Decode("sample", []byte(registeredCrashProfile))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := config.BuildCatalog([]profile.Metadata{declaration}, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := resource.ParseResource("sample", "server", strings.NewReader("COMMAND=server\nHANDLER=handler.sh\nTITLE=sample\nPROXY=\"sample-server SAMPLE_HOST SAMPLE_PORT tcp:127.0.0.1:9119 owner-metadata-v1 loopback-or-tailscale\"\nACTION=\"up up security-change reversible\"\nACTION=\"down down security-change reversible\"\nBRINGUP=up\nSHUTDOWN=down\n"))
	if err != nil {
		t.Fatal(err)
	}
	resources, err := resource.BuildRegistry([]resource.ResourceSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	yard := domain.Context{YardName: "default", IncusProject: "fixture", YardInstanceName: "yard", IncusBridge: "fixture-br", AccessKind: domain.AccessLocal, SSHPort: 2240, Paths: domain.RuntimePaths{ConfigHome: home, DataHome: home + "/data"}}
	if native.vm {
		yard.YardKind = domain.YardVM
	}
	load := func(_ context.Context, view rt.V2SettingsSnapshotView) ([]migration.RegisteredYardTarget, error) {
		snapshot, err := config.ReadPersistentFileSnapshot(home, settings)
		if view != nil {
			snapshot, err = view.ReadSnapshot(settings)
		}
		if err != nil {
			return nil, err
		}
		assignments, err := config.ParsePersistentAssignments(settings, snapshot.Content)
		if err != nil {
			return nil, err
		}
		values := map[string]string{"ENVIRONMENT_PROFILES": "sample", "SAMPLE_HOST": "localhost", "SAMPLE_PORT": "31000"}
		authored := map[string]bool{}
		for _, assignment := range assignments {
			values[assignment.Name] = assignment.Value
			authored[assignment.Name] = true
		}
		traces := map[string]config.SettingTrace{}
		for _, key := range []string{"SAMPLE_HOST", "SAMPLE_PORT", "ENVIRONMENT_PROFILES"} {
			resolution := config.SettingResolution{Scope: "default", Role: "shipped defaults", Status: "effective"}
			if authored[key] {
				resolution = config.SettingResolution{Scope: "yard", Role: "local settings", Status: "effective"}
			}
			traces[key] = config.SettingTrace{EffectiveValue: values[key], Resolutions: []config.SettingResolution{resolution}}
		}
		loaded := config.Loaded{Catalog: catalog, Context: yard, Environment: values, Settings: traces, ConfigurationLayers: []config.ConfigurationLayer{{Path: settings, Present: true}}}
		return []migration.RegisteredYardTarget{{Loaded: loaded, Runtime: reconcileruntime.Runtime{RepositoryRoot: root, Incus: native, Executor: native, Yard: yard}}}, nil
	}
	owner := &migration.RegisteredYardMigration{Yard: "default", ConfigHome: home, Resources: resources, LoadTargets: load}
	tmp := &migration.RegisteredYardMigration{Yard: "default", ConfigHome: home, Resources: resources, GuestTmp: true, LoadTargets: load}
	links, err := rt.NewRuntimeLinkStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	options := rt.V2Options{CandidateConfigDir: candidate, ConfigHome: home, RegistryPayload: []byte(registeredCrashRegistry), ArtifactDigest: crashPin("artifact"), Releases: rt.ReleasePair{From: "release-a", Target: "release-a"}, Direction: rt.DirectionActivateTarget,
		ObserveLinks:               func(context.Context) (rt.ReleaseLinks, error) { return rt.ReleaseLinks{Active: "release-a"}, nil },
		RegisteredEndpointSettings: resources.EndpointSettingNames(), RegisteredYards: []rt.V2ActivationReconciler{owner}, GuestDiskTmp: []rt.V2ActivationReconciler{tmp},
		VerifyAuthorization: func(plan rt.PlanToken, grant rt.Authorization) bool { return string(grant) == "grant:"+string(plan) }}
	if family == crashFamilyRegisteredRevision {
		options.Reconcilers = []rt.V2ActivationReconciler{&testVMsRevisionActivation{native: native, trace: trace}}
	}
	return &crashFixture{family: family, target: "release-a", home: home, root: root, registry: []byte(registeredCrashRegistry), options: options, links: links, trace: trace, signature: registeredCrashSignature}
}

func mustMkdirAll(t *testing.T, path string, create bool) string {
	t.Helper()
	if create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func registeredCrashSignature(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, relative := range []string{"config/yards/default/config.env", "config/.sync/settings/config.env", "native.json"} {
		body, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		result[relative] = string(body)
	}
	var ledger struct {
		Domains json.RawMessage `json:"domains"`
	}
	body, err := os.ReadFile(filepath.Join(root, "config/release-transition/v2/ledger.json"))
	if err == nil {
		err = json.Unmarshal(body, &ledger)
	}
	if err != nil {
		t.Fatal(err)
	}
	result["ledger/domains"] = string(ledger.Domains)
	return result
}

func TestProcessCrashRegisteredRowsResumeFresh(t *testing.T) {
	runCrashMatrix(t, newRegisteredCrashFixture, func(t *testing.T, cuts map[string]bool) {
		for _, cut := range []string{"native/registered/receipts", "native/guest-tmp/apply", "native/guest-tmp/marker"} {
			if !cuts[cut] {
				t.Fatalf("control did not reach %s: %v", cut, cuts)
			}
		}
	})
}

// testVMsRevisionActivation has the effect of the test VM backend reconcile on a
// real yard (internal/adapters/testvmsruntime/backend.go): activation rewrites the
// release-owned revision in instance config after every native row is Verified.
type testVMsRevisionActivation struct {
	native *registeredCrashNative
	trace  *crashTrace
}

func (*testVMsRevisionActivation) ID() string { return "test-vms-revision" }

func (actor *testVMsRevisionActivation) Observe(context.Context, rt.ReleasePair, rt.ReleaseLinks) (rt.V2ActivationObservation, error) {
	state, err := actor.native.read()
	if err != nil {
		return rt.V2ActivationObservation{}, err
	}
	actor.native.load(state)
	ready := state.Instance.LocalConfig[testVMsRevisionKey] == "revision-after"
	actual := crashPin("revision-before")
	if ready {
		actual = crashPin("revision-after")
	}
	return rt.V2ActivationObservation{Actual: actual, Desired: crashPin("revision-after"), Converged: ready}, nil
}

func (actor *testVMsRevisionActivation) Reconcile(context.Context, rt.ReleaseLinks) error {
	state, err := actor.native.read()
	if err != nil {
		return err
	}
	state.Instance.Config[testVMsRevisionKey] = "revision-after"
	state.Instance.LocalConfig[testVMsRevisionKey] = "revision-after"
	if err := actor.native.write(state); err != nil {
		return err
	}
	return actor.trace.hit("native/activation/revision")
}

// Activation rewrites what a finished row bound, then fails before Complete. The
// resume must finish the same transaction instead of ending in a terminal refusal.
func TestProcessCrashRegisteredActivationWriteResumes(t *testing.T) {
	runCrashMatrix(t, registeredCrashFamily(crashFamilyRegisteredRevision), func(t *testing.T, cuts map[string]bool) {
		for _, cut := range []string{"native/activation/revision", "global/before-journal-complete"} {
			if !cuts[cut] {
				t.Fatalf("control did not reach %s: %v", cut, cuts)
			}
		}
	})
}

// Every cut of a VM yard resumes across its restart outputs, including the last
// row, which is still fingerprinted.
func TestProcessCrashRegisteredVMRowsResumeFresh(t *testing.T) {
	runCrashMatrix(t, registeredCrashFamily(crashFamilyRegisteredVM), func(t *testing.T, cuts map[string]bool) {
		if !cuts["native/guest-tmp/apply"] {
			t.Fatalf("control did not reach the guest restart: %v", cuts)
		}
	})
}

// A yard that changes under a row which is still pinned must refuse: the first row
// is covered by the pending row after it, and the last row by its own fingerprint.
func TestProcessCrashRegisteredIdentityDriftRefusesResume(t *testing.T) {
	for _, test := range []struct{ name, family, cut, key, value string }{
		{"unchanged yard passes", crashFamilyRegistered, "row/guest-tmp-v3.yard.default/verified", "", ""},
		{"replaced at first row", crashFamilyRegistered, "row/registered-v3.yard.default/verified", "volatile.uuid", "replaced"},
		{"replaced at last row", crashFamilyRegistered, "row/guest-tmp-v3.yard.default/verified", "volatile.uuid", "replaced"},
		{"limits changed at first row", crashFamilyRegistered, "row/registered-v3.yard.default/verified", "limits.cpu", "4"},
		{"limits changed at last row", crashFamilyRegistered, "row/guest-tmp-v3.yard.default/verified", "limits.cpu", "4"},
		{"foreign disk tmp marker at last row", crashFamilyRegistered, "row/guest-tmp-v3.yard.default/verified", "user.subyard.disk_tmp", "v2"},
		{"VM replaced at last row", crashFamilyRegisteredVM, "row/guest-tmp-v3.yard.default/verified", "volatile.uuid", "replaced"},
		{"VM foreign reset value at last row", crashFamilyRegisteredVM, "row/guest-tmp-v3.yard.default/verified", "volatile.vm.needs_reset", "false"},
		{"VM foreign clock at last row", crashFamilyRegisteredVM, "row/guest-tmp-v3.yard.default/verified", "volatile.vm.rtc_adjustment", "01"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := registeredCrashFamily(test.family)(t)
			crashChild(t, f, test.cut, false, true)
			statePath := filepath.Join(f.root, "native.json")
			body, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			var state registeredCrashState
			if err := json.Unmarshal(body, &state); err != nil {
				t.Fatal(err)
			}
			if test.key != "" {
				state.Instance.Config[test.key] = test.value
				state.Instance.LocalConfig[test.key] = test.value
			}
			if body, err = json.Marshal(state); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, statePath, body, 0o600)
			fresh := registeredCrashFixtureAt(t, f.root, false, test.family)
			transition, err := rt.NewV2Transition(fresh.options)
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := transition.Inspect(context.Background(), rt.Goal{Target: rt.ReleaseID(fresh.target), Direction: rt.DirectionActivateTarget})
			if test.key == "" {
				if err != nil || len(inspection.Blockers) != 0 {
					t.Fatalf("unchanged yard refused: %+v %v", inspection, err)
				}
				return
			}
			if err == nil && len(inspection.Blockers) == 0 {
				t.Fatalf("drifted yard passed as a finished row: %+v", inspection.Outcome)
			}
		})
	}
}
