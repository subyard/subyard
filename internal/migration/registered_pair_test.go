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
func TestBothConcreteRegisteredOwnersShareOneDurableJournal(t *testing.T) {
	for _, cut := range []string{"complete", "after-ssh", "after-endpoint", "marker-cut", "vm-rtc-cut", "unrelated-after-endpoint", "inherited-after-endpoint", "implicit-default-no-endpoint"} {
		t.Run(cut, func(t *testing.T) {
			owner, tmp, native, path := pairedRegisteredOwners(t)
			if cut == "vm-rtc-cut" {
				instance := native.Instances["fixture/yard"]
				instance.Type = domain.YardVM
				for _, values := range []map[string]string{instance.Config, instance.LocalConfig} {
					values["volatile.vm.rtc_adjustment"], values["volatile.vm.rtc_offset"] = "0", "0"
				}
				native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
				original := owner.LoadTargets
				loader := func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
					targets, err := original(ctx, view)
					for index := range targets {
						targets[index].Loaded.Context.YardKind = domain.YardVM
						targets[index].Runtime.Yard.YardKind = domain.YardVM
					}
					return targets, err
				}
				owner.LoadTargets, tmp.LoadTargets = loader, loader
			}
			candidate := registeredSSHConfigFixture(t)
			gitPath := filepath.Join(owner.ConfigHome, ".sync/settings/config.env")
			if err := os.MkdirAll(filepath.Dir(gitPath), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, gitPath, []byte("SSH_PORT=2240\n"), 0600)
			implicit := cut == "implicit-default-no-endpoint"
			if implicit {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, gitPath, []byte("# No legacy SSH override.\n"), 0600)
				instance := native.Instances["fixture/yard"]
				instance.Devices, instance.LocalDevices = map[string]map[string]string{}, map[string]map[string]string{}
				native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
			}
			original := owner.LoadTargets
			activeCut := ""
			loader := func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
				assignments, err := config.ReadAssignments(path)
				if err != nil && !(implicit && errors.Is(err, os.ErrNotExist)) {
					return nil, err
				}
				ssh := assignments["SSH_PORT"] == "2240"
				endpoint := assignments["SAMPLE_PORT"] == "32000"
				if activeCut == "after-ssh" && ssh && !endpoint || activeCut == "after-endpoint" && endpoint {
					return nil, errors.New("durable settings cut")
				}
				targets, err := original(ctx, view)
				for index := range targets {
					targets[index].Loaded.Context.SSHPort = 2240
					targets[index].Runtime.Yard.SSHPort = 2240
				}
				return targets, err
			}
			owner.LoadTargets, tmp.LoadTargets = loader, loader
			payload := []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":2,"power-metadata":2,"settings":2,"project-state":1},"currentEpochs":{"owner-registration":3,"power-metadata":3,"settings":3,"project-state":1},"migrations":[{"id":"local-ssh-v3","domain":"settings","fromEpoch":2,"toEpoch":3,"kind":"local-ssh-port-settings-v2-to-v3"},{"id":"registered-v3","domain":"owner-registration","fromEpoch":2,"toEpoch":3,"kind":"registered-yards-v2-to-v3","dependsOn":["local-ssh-v3"]},{"id":"guest-tmp-v3","domain":"power-metadata","fromEpoch":2,"toEpoch":3,"kind":"registered-guest-disk-tmp-v2-to-v3","dependsOn":["registered-v3"]}]}`)
			options := releasetransition.V2Options{CandidateConfigDir: candidate, ConfigHome: owner.ConfigHome, RegistryPayload: payload, ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)), Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"}, Direction: releasetransition.DirectionActivateTarget, ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
				return releasetransition.ReleaseLinks{Active: "release-a"}, nil
			}, RegisteredEndpointSettings: owner.Resources.EndpointSettingNames(), RegisteredYards: []releasetransition.V2ActivationReconciler{owner}, GuestDiskTmp: []releasetransition.V2ActivationReconciler{tmp}, VerifyAuthorization: func(_ releasetransition.PlanToken, grant releasetransition.Authorization) bool {
				return grant == "confirmed"
			}}
			if implicit {
				options.RegisteredEndpointSettings = nil
			}
			transition, err := releasetransition.NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			goal := releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil || len(inspection.Blockers) != 0 {
				t.Fatalf("inspect: %#v %v", inspection, err)
			}
			if _, err := os.Lstat(filepath.Join(owner.ConfigHome, "host-id")); !errors.Is(err, os.ErrNotExist) || native.grants != 0 {
				t.Fatal("read-only inspection created owner identity or native grants", err)
			}
			native.failTmp = cut == "marker-cut" || cut == "vm-rtc-cut"
			activeCut = cut
			if strings.HasSuffix(cut, "-after-endpoint") {
				activeCut = "after-endpoint"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			native.cancelTmp = cancel
			outcome, err := transition.Converge(ctx, releasetransition.Execution{Plan: inspection.Plan, Authorization: "confirmed"})
			if cut != "complete" && !implicit {
				if err == nil && outcome.Status == releasetransition.StatusReady {
					t.Fatal("cut was completed")
				}
				if (cut == "marker-cut" || cut == "vm-rtc-cut") && native.Instances["fixture/yard"].LocalConfig["user.subyard.resource.sample-server"] == "" {
					t.Fatalf("registration did not finish before cut: %#v %v", outcome, err)
				}
				if cut == "unrelated-after-endpoint" {
					content, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, path, append(content, []byte("UNRELATED=value\n")...), 0600)
				}
				if cut == "inherited-after-endpoint" {
					testkit.WriteFile(t, gitPath, []byte("SSH_PORT=2241\n"), 0600)
				}
				if cut == "vm-rtc-cut" {
					// Incus updates its clock bookkeeping and pending-reset flag across a real
					// VM stop/start. Model only those lifecycle outputs before a fresh process
					// resumes the same durable migration; policy readiness uses the existing leaf fixture.
					instance := native.Instances["fixture/yard"]
					for _, values := range []map[string]string{instance.Config, instance.LocalConfig} {
						values["volatile.vm.rtc_adjustment"], values["volatile.vm.rtc_offset"] = "1", "-1"
						values["volatile.vm.needs_reset"] = "true"
					}
					native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
				}
				reopened := *owner
				reopened.prepared = nil
				reopened.observedActual = ""
				reopened.observedDesired = ""
				options.RegisteredYards = []releasetransition.V2ActivationReconciler{&reopened}
				options.GuestDiskTmp = []releasetransition.V2ActivationReconciler{&RegisteredYardMigration{Yard: tmp.Yard, ConfigHome: tmp.ConfigHome, Resources: tmp.Resources, GuestTmp: true, LoadTargets: tmp.LoadTargets}}
				native.failTmp = false
				activeCut = ""
				transition, err = releasetransition.NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
				inspection, err = transition.Inspect(context.Background(), goal)
				if strings.HasSuffix(cut, "-after-endpoint") {
					if err == nil && len(inspection.Blockers) == 0 {
						t.Fatal("unrelated authorized input drift accepted")
					}
					if native.grants != 0 {
						t.Fatal("native grant preceded drift refusal")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				outcome, err = transition.Converge(context.Background(), releasetransition.Execution{Plan: inspection.Plan, Authorization: "confirmed"})
			}
			if err != nil || outcome.Status != releasetransition.StatusReady {
				t.Fatalf("converge: %#v %v", outcome, err)
			}
			if _, err := os.Lstat(filepath.Join(owner.ConfigHome, "host-id")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("compatibility migration created an unrelated owner HostID", err)
			}
			values, err := config.ReadAssignments(path)
			if !implicit && (err != nil || values["SSH_PORT"] != "2240" || values["SAMPLE_HOST"] != "127.0.0.1" || values["SAMPLE_PORT"] != "32000") {
				t.Fatalf("retained endpoint: %#v %v", values, err)
			}
			if implicit {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unneeded default settings file created", err)
				}
			}
			if native.Instances["fixture/yard"].Config["user.subyard.disk_tmp"] != "v1" {
				t.Fatal("tmp receipt absent")
			}
			store, err := releasetransition.NewPOSIXV2Store(owner.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			registry, digest, err := releasetransition.ParseRegistryV2(payload, releasetransition.BuiltinCapabilityCatalog())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.InspectSourceMetadata(registry, digest, options.RegisteredEndpointSettings...); err != nil {
				t.Fatal("durable source admission", err)
			}
			beforeAdmission, err := store.ReadCurrentJournal()
			if err != nil {
				t.Fatal(err)
			}
			unprovenNames := [][]string{nil, {"SSH_PORT"}}
			if implicit {
				unprovenNames = nil
			}
			for _, unproven := range unprovenNames {
				if _, err := store.InspectSourceMetadata(registry, digest, unproven...); err == nil {
					t.Fatal("source history admitted without original descriptor vocabulary")
				}
			}
			afterAdmission, err := store.ReadCurrentJournal()
			if err != nil || string(beforeAdmission.Payload) != string(afterAdmission.Payload) {
				t.Fatal("source inspection changed journal")
			}

			if implicit {
				freshOwner, freshTmp := *owner, *tmp
				freshOwner.prepared, freshTmp.prepared = nil, nil
				freshOwner.observedActual, freshOwner.observedDesired = "", ""
				freshTmp.observedActual, freshTmp.observedDesired = "", ""
				options.RegisteredYards = []releasetransition.V2ActivationReconciler{&freshOwner}
				options.GuestDiskTmp = []releasetransition.V2ActivationReconciler{&freshTmp}
				transition, err = releasetransition.NewV2Transition(options)
				if err != nil {
					t.Fatal(err)
				}
			}
			repeat, err := transition.Inspect(context.Background(), goal)
			if err != nil || repeat.Assessment.Changed {
				t.Fatalf("repeat: %#v %v", repeat, err)
			}
		})
	}
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
	loaded := config.Loaded{Context: domain.Context{YardName: "named", Paths: domain.RuntimePaths{ConfigHome: home, ConfigDir: home + "/shipped/config"}}}
	path, err := registeredSettingsPath(loaded)
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
	registration, err := config.FindYardRegistrationFile(loaded.Context.Paths.ConfigDir, home, "named")
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

type aliasRetryNative struct {
	*registeredNativeFixture
	applies int
	cancel  context.CancelFunc
}

func (n *aliasRetryNative) Exec(ctx context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	action := request.Command[len(request.Command)-1]
	if action == "apply" {
		n.applies++
		if n.cancel != nil {
			cancel := n.cancel
			n.cancel = nil
			cancel()
			return ports.InstanceExecResult{}, context.Canceled
		}
	}
	if action == "check" || action == "assess" {
		return ports.InstanceExecResult{ExitCode: 10}, &ports.GuestExitError{Code: 10}
	}
	return n.registeredNativeFixture.Exec(ctx, project, name, request)
}
func TestRegisteredMigrationResumesAcrossVerifiedAssetAliases(t *testing.T) {
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
	executor := &aliasRetryNative{registeredNativeFixture: native}
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
			loaded, err := config.Load(config.LoadOptions{RepositoryRoot: alias, OperatorHome: base.ConfigHome, YardName: "default", DisablePrivate: true, ProspectiveFiles: files, Environment: map[string]string{"HOME": base.ConfigHome, "SUBYARD_OPERATOR_HOME": base.ConfigHome, "SUBYARD_CONFIG_HOME": base.ConfigHome, "SUBYARD_HOME": base.ConfigHome + "/data", "INCUS_PROJECT": "fixture", "INCUS_BRIDGE": "fixture-br", "YARD_INSTANCE_NAME": "yard", "CODING_TOOL_INTEGRATIONS": "codex"}})
			if err != nil {
				return nil, err
			}
			runtime := reconcileruntime.Runtime{RepositoryRoot: alias, Incus: native, Executor: executor, NetworkPolicy: restartNetwork{}, Yard: loaded.Context}
			return []RegisteredYardTarget{{Loaded: loaded, Runtime: runtime}}, nil
		}
		owner := &RegisteredYardMigration{Yard: "default", ConfigHome: base.ConfigHome, Resources: base.Resources, LoadTargets: load}
		tmp := &RegisteredYardMigration{Yard: "default", ConfigHome: base.ConfigHome, Resources: base.Resources, LoadTargets: load, GuestTmp: true}
		return owner, tmp, prompt
	}
	owner, tmp, prompt := makeOwners(aliases[0])
	payload := []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":2,"power-metadata":2},"currentEpochs":{"owner-registration":3,"power-metadata":3},"migrations":[{"id":"registered-v3","domain":"owner-registration","fromEpoch":2,"toEpoch":3,"kind":"registered-yards-v2-to-v3"},{"id":"guest-tmp-v3","domain":"power-metadata","fromEpoch":2,"toEpoch":3,"kind":"registered-guest-disk-tmp-v2-to-v3","dependsOn":["registered-v3"]}]}`)
	options := releasetransition.V2Options{CandidateConfigDir: aliases[0] + "/config", ConfigHome: base.ConfigHome, RegistryPayload: payload, ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)), Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"}, Direction: releasetransition.DirectionActivateTarget, ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
		return releasetransition.ReleaseLinks{Active: "release-a"}, nil
	}, RegisteredEndpointSettings: base.Resources.EndpointSettingNames(), RegisteredYards: []releasetransition.V2ActivationReconciler{owner}, GuestDiskTmp: []releasetransition.V2ActivationReconciler{tmp}, VerifyAuthorization: func(_ releasetransition.PlanToken, a releasetransition.Authorization) bool { return a == "confirmed" }}
	goal := releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget}
	transition, err := releasetransition.NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("initial inspect: %#v %v", inspection, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	executor.cancel = cancel
	_, _ = transition.Converge(ctx, releasetransition.Execution{Plan: inspection.Plan, Authorization: "confirmed"})
	cancel()
	if len(prompt.Requests) != 0 || executor.applies != 1 {
		t.Fatalf("did not reach first interruption: prompts=%d apply=%d", len(prompt.Requests), executor.applies)
	}
	store, err := releasetransition.NewPOSIXV2Store(base.ConfigHome)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadCurrentJournal()
	if err != nil || !before.Exists {
		t.Fatal("missing durable interrupted journal", err)
	}
	owner, tmp, prompt = makeOwners(aliases[1])
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

	options.CandidateConfigDir = aliases[1] + "/config"
	options.RegisteredYards = []releasetransition.V2ActivationReconciler{owner}
	options.GuestDiskTmp = []releasetransition.V2ActivationReconciler{tmp}
	transition, err = releasetransition.NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err = transition.Inspect(context.Background(), goal)
	if err != nil || inspection.Resume == nil || len(inspection.Blockers) != 0 {
		t.Fatalf("reopened inspect: %#v %v", inspection, err)
	}
	_, _ = transition.Converge(context.Background(), releasetransition.Execution{Plan: inspection.Plan, Authorization: "confirmed"})
	if len(prompt.Requests) != 0 || executor.applies != 2 {
		t.Fatalf("did not reach resumed native attempt: prompts=%d apply=%d", len(prompt.Requests), executor.applies)
	}
	after, err := store.ReadCurrentJournal()
	if err != nil {
		t.Fatal(err)
	}
	oldJournal, err := releasetransition.ParseJournal(before.Payload)
	if err != nil {
		t.Fatal(err)
	}
	newJournal, err := releasetransition.ParseJournal(after.Payload)
	if err != nil || oldJournal.Transaction != newJournal.Transaction {
		t.Fatal("retry replaced interrupted transaction", err)
	}
}

func TestRegisteredDesiredIgnoresVMRestartOutputs(t *testing.T) {
	for _, guest := range []bool{false, true} {
		t.Run(fmt.Sprintf("guest-%t", guest), func(t *testing.T) {
			owner, tmp, native, _ := pairedRegisteredOwners(t)
			if guest {
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
			before := desired()
			set("volatile.eth0.host_name", "tapAFTER")
			set("volatile.vm.rtc_adjustment", "-2")
			set("volatile.vm.needs_reset", "true")
			if desired() != before {
				t.Fatal("VM restart outputs changed the pinned Desired")
			}
		})
	}
}

func TestRegisteredMigrationBindsNonClockVMDrift(t *testing.T) {
	for _, guest := range []bool{false, true} {
		for _, change := range []string{"malformed-clock", "noncanonical-clock", "clock-suffix", "container-clock", "reset-false", "reset-empty", "reset-suffix", "container-reset", "uuid", "generation", "vsock", "limits", "device"} {
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
				case "limits":
					key = "limits.cpu"
				case "device":
					instance.Devices = maps.Clone(instance.Devices)
					instance.Devices["foreign"] = map[string]string{"type": "disk", "source": "/foreign", "path": "/foreign"}
				}
				if change != "device" {
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

type cutActivation struct{ cut bool }

func (*cutActivation) ID() string { return "activation-cut" }
func (activation *cutActivation) Observe(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks) (releasetransition.V2ActivationObservation, error) {
	desired := releasetransition.Fingerprint(strings.Repeat("a", 64))
	if activation.cut {
		return releasetransition.V2ActivationObservation{Actual: releasetransition.Fingerprint(strings.Repeat("b", 64)), Desired: desired}, nil
	}
	return releasetransition.V2ActivationObservation{Actual: desired, Desired: desired, Converged: true}, nil
}
func (activation *cutActivation) Reconcile(context.Context, releasetransition.ReleaseLinks) error {
	if activation.cut {
		return errors.New("activation cut")
	}
	return nil
}

// A later process must resume activation after both native steps were verified.
// Each owner's recorded Desired has to hold after the other owner's published result.
func TestFreshProcessResumesActivationAfterBothRegisteredOwnersVerified(t *testing.T) {
	owner, tmp, native, _ := pairedRegisteredOwners(t)
	candidate := registeredSSHConfigFixture(t)
	payload := []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":2,"power-metadata":2},"currentEpochs":{"owner-registration":3,"power-metadata":3},"migrations":[{"id":"registered-v3","domain":"owner-registration","fromEpoch":2,"toEpoch":3,"kind":"registered-yards-v2-to-v3"},{"id":"guest-tmp-v3","domain":"power-metadata","fromEpoch":2,"toEpoch":3,"kind":"registered-guest-disk-tmp-v2-to-v3","dependsOn":["registered-v3"]}]}`)
	activation := &cutActivation{cut: true}
	options := releasetransition.V2Options{CandidateConfigDir: candidate, ConfigHome: owner.ConfigHome, RegistryPayload: payload, ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
		Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"}, Direction: releasetransition.DirectionActivateTarget,
		ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
			return releasetransition.ReleaseLinks{Active: "release-a"}, nil
		},
		RegisteredEndpointSettings: owner.Resources.EndpointSettingNames(),
		RegisteredYards:            []releasetransition.V2ActivationReconciler{owner}, GuestDiskTmp: []releasetransition.V2ActivationReconciler{tmp},
		Reconcilers: []releasetransition.V2ActivationReconciler{activation},
		VerifyAuthorization: func(_ releasetransition.PlanToken, grant releasetransition.Authorization) bool {
			return grant == "confirmed"
		},
	}
	transition, err := releasetransition.NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	goal := releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget}
	inspection, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(inspection.Blockers) != 0 {
		t.Fatalf("inspect: %#v %v", inspection, err)
	}
	if outcome, err := transition.Converge(context.Background(), releasetransition.Execution{Plan: inspection.Plan, Authorization: "confirmed"}); err == nil && outcome.Status == releasetransition.StatusReady {
		t.Fatal("cut activation reported ready")
	}
	if native.Instances["fixture/yard"].LocalConfig["user.subyard.disk_tmp"] != "v1" {
		t.Fatal("guest migration did not publish its marker before the cut")
	}
	reopened, reopenedTmp := *owner, *tmp
	reopened.prepared, reopened.observedActual, reopened.observedDesired = nil, "", ""
	reopenedTmp.prepared, reopenedTmp.observedActual, reopenedTmp.observedDesired = nil, "", ""
	options.RegisteredYards = []releasetransition.V2ActivationReconciler{&reopened}
	options.GuestDiskTmp = []releasetransition.V2ActivationReconciler{&reopenedTmp}
	activation.cut = false
	transition, err = releasetransition.NewV2Transition(options)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := transition.Inspect(context.Background(), goal)
	if err != nil || len(resumed.Blockers) != 0 || resumed.Resume == nil {
		t.Fatalf("fresh process could not resume: %#v %v", resumed.Blockers, err)
	}
	outcome, err := transition.Converge(context.Background(), releasetransition.Execution{Plan: resumed.Plan, Authorization: "confirmed"})
	if err != nil || outcome.Status != releasetransition.StatusReady {
		t.Fatalf("resume: %#v %v", outcome, err)
	}
}
