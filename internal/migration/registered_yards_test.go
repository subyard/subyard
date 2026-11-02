package migration

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

type registeredNativeFixture struct {
	*testkit.Incus
	grants  int
	failTmp bool
}

func (native *registeredNativeFixture) AdoptRegisteredInstance(_ context.Context, before ports.InstanceInfo, values map[string]string) error {
	if native.failTmp && values["user.subyard.disk_tmp"] == "v1" {
		return fmt.Errorf("injected tmp marker cut")
	}
	current := native.Instances[before.Project+"/"+before.Name]
	for key, value := range values {
		current.LocalConfig[key] = value
		current.Config[key] = value
	}
	native.Instances[before.Project+"/"+before.Name] = current
	native.grants++
	return nil
}
func (native *registeredNativeFixture) Exec(_ context.Context, project, name string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	native.ExecCalls = append(native.ExecCalls, testkit.IncusExecCall{Project: project, Name: name, Request: request})
	return ports.InstanceExecResult{}, nil
}
func registeredMigrationFixture(t *testing.T) (*RegisteredYardMigration, *registeredNativeFixture, string) {
	t.Helper()
	home := testkit.TempDir(t)
	path := filepath.Join(home, "yards/default/config.env")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte("ENVIRONMENT_PROFILES=sample\n"), 0600)
	declaration, err := profile.Decode("sample", []byte(`{"schema_version":1,"settings":[{"name":"SAMPLE_HOST","type":"string","scopes":["yard","shipped"],"application":"next-command","syncable":true},{"name":"SAMPLE_PORT","type":"port","minimum":1,"maximum":65535,"scopes":["yard","shipped"],"application":"next-command","syncable":true}]}`))
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
	yard := domain.Context{YardName: "default", IncusProject: "fixture", YardInstanceName: "yard", IncusBridge: "fixture-br", AccessKind: domain.AccessLocal, Paths: domain.RuntimePaths{ConfigHome: home, DataHome: home + "/data"}}
	values := map[string]string{"volatile.uuid": "uuid-original", "user.subyard.managed": "true", "user.subyard.initialized": "true", "user.subyard.name": "default", "user.subyard.bridge": "fixture-br", "user.subyard.desired_power": "running"}
	devices := map[string]map[string]string{"sample-server": {"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:32000", "connect": "tcp:127.0.0.1:9119"}}
	instance := ports.InstanceInfo{Project: "fixture", Name: "yard", Status: "Running", Config: values, LocalConfig: maps.Clone(values), Devices: devices, LocalDevices: maps.Clone(devices)}
	native := &registeredNativeFixture{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": instance}, Reconcile: ports.ReconcileState{ProjectFound: true, InstanceFound: true, Instance: instance, ProjectConfig: map[string]string{"user.subyard.installation": "1", "user.subyard.operator_uid": strconv.Itoa(os.Getuid()), "user.subyard.config_home": home, "user.subyard.data_home": home + "/data"}}}}
	loaded := config.Loaded{Catalog: catalog, Context: yard, Environment: map[string]string{"ENVIRONMENT_PROFILES": "sample", "SAMPLE_HOST": "127.0.0.1", "SAMPLE_PORT": "31000"}, Settings: map[string]config.SettingTrace{"SAMPLE_HOST": {EffectiveValue: "127.0.0.1"}, "SAMPLE_PORT": {EffectiveValue: "31000", Resolutions: []config.SettingResolution{{Scope: "default", Role: "shipped defaults", Status: "effective"}}}}}
	runtime := reconcileruntime.Runtime{Incus: native, Yard: yard}
	migration := &RegisteredYardMigration{Yard: "default", ConfigHome: home, Resources: resources, LoadTargets: func(context.Context, releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
		return []RegisteredYardTarget{{Loaded: loaded, Runtime: runtime}}, nil
	}}
	return migration, native, path
}
func TestRegisteredMigrationPreservesWorkingDeclaredEndpointAndBindsDrift(t *testing.T) {
	for _, scenario := range []string{"complete", "route-drift", "source-drift", "foreign-receipt", "idmap-drift"} {
		t.Run(scenario, func(t *testing.T) {
			migration, native, path := registeredMigrationFixture(t)
			initial, err := migration.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if err != nil || initial.Converged {
				t.Fatalf("initial: %#v %v", initial, err)
			}
			if scenario == "route-drift" {
				native.Instances["fixture/yard"].LocalDevices["sample-server"]["listen"] = "tcp:127.0.0.1:32001"
			}
			if scenario == "idmap-drift" {
				native.Instances["fixture/yard"].LocalConfig["volatile.idmap.current"] = "different-safe-shape"
			}
			if scenario == "source-drift" {
				testkit.WriteFile(t, path, []byte("ENVIRONMENT_PROFILES=sample\nUNRELATED=value\n"), 0600)
			}
			if scenario == "foreign-receipt" {
				native.Instances["fixture/yard"].LocalConfig["user.subyard.resource.sample-server"] = "foreign"
			}
			observed, err := migration.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if scenario != "complete" {
				if err == nil && observed.Actual == initial.Actual && observed.Desired == initial.Desired {
					t.Fatal("drift did not change bound proof")
				}
				return
			}
			if err := migration.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal(err)
			}
			final, err := migration.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if err != nil || !final.Converged || final.Desired != initial.Desired || native.grants != 1 {
				t.Fatalf("final: %#v grants=%d %v", final, native.grants, err)
			}
			content, _ := os.ReadFile(path)
			if strings.Contains(string(content), "32000") || native.Instances["fixture/yard"].LocalDevices["sample-server"]["listen"] != "tcp:127.0.0.1:32000" {
				t.Fatal("working endpoint was changed", fmt.Sprint(content))
			}
		})
	}
}

func TestRegisteredOwnersBindOptionalHostID(t *testing.T) {
	for _, guestTmp := range []bool{false, true} {
		for _, change := range []string{"created", "removed", "unsafe-mode", "unsafe-content", "symlink"} {
			t.Run(fmt.Sprintf("guest-tmp-%t/%s", guestTmp, change), func(t *testing.T) {
				owner, tmp, native, _ := pairedRegisteredOwners(t)
				if guestTmp {
					owner = tmp
				}
				path := filepath.Join(owner.ConfigHome, "host-id")
				if change == "removed" {
					testkit.WriteFile(t, path, []byte("fixture-owner\n"), 0600)
				}
				if _, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{}); err != nil {
					t.Fatal(err)
				}
				switch change {
				case "removed":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink("missing-owner", path); err != nil {
						t.Fatal(err)
					}
				default:
					content, mode := []byte("fixture-owner\n"), os.FileMode(0600)
					if change == "unsafe-mode" {
						mode = 0644
					}
					if change == "unsafe-content" {
						content = []byte("unsafe/owner\n")
					}
					testkit.WriteFile(t, path, content, mode)
				}
				err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{})
				if err == nil || native.grants != 0 {
					t.Fatalf("HostID drift or unsafe source received mutation: err=%v grants=%d", err, native.grants)
				}
				if change == "created" || change == "removed" {
					if !errors.Is(err, domain.ErrPlanStale) {
						t.Fatalf("HostID presence drift must stale the plan: %v", err)
					}
				} else if _, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{}); err == nil {
					t.Fatal("unsafe existing HostID admitted by fresh inspection")
				}
			})
		}
	}
}

func TestRegisteredOwnershipConsequencesDescribeOnlyNewAuthority(t *testing.T) {
	owner, native, _ := registeredMigrationFixture(t)
	instance := native.Instances["fixture/yard"]
	contract := owner.Resources.ProxyContracts([]string{"sample"})[0]
	for _, values := range []map[string]string{instance.Config, instance.LocalConfig} {
		values[contract.OwnershipKey()] = contract.OwnershipValue(instance.LocalDevices[contract.Device])
	}
	native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
	plans, err := owner.plans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	observed, err := owner.observePlans(context.Background(), plans)
	if err != nil || len(observed.Consequences) != 0 {
		t.Fatalf("existing authority disclosed as new: %#v %v", observed.Consequences, err)
	}
	plans[0].metadata = map[string]string{"user.subyard.installation": "1"}
	plans[0].before = map[string]string{}
	observed, err = owner.observePlans(context.Background(), plans)
	if err != nil || len(observed.Consequences) != 1 || !strings.Contains(observed.Consequences[0], "project fixture") || !strings.Contains(observed.Consequences[0], "removal") {
		t.Fatalf("project grant not disclosed: %#v %v", observed.Consequences, err)
	}
}
func TestRegisteredDesiredIgnoresDeviceLifecycleOutputs(t *testing.T) {
	owner, native, _ := registeredMigrationFixture(t)
	instance := native.Instances["fixture/yard"]
	nic := map[string]string{"type": "nic", "network": "fixture-br"}
	instance.LocalDevices["eth1"], instance.Devices["eth1"] = nic, maps.Clone(nic)
	set := func(key, value string) {
		instance.LocalConfig[key], instance.Config[key] = value, value
		native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
	}
	desired := func() releasetransition.Fingerprint {
		t.Helper()
		observed, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
		if err != nil {
			t.Fatal(err)
		}
		return observed.Desired
	}
	set("volatile.eth1.host_name", "vethAAAA")
	set("volatile.eth1.last_state.created", "false")
	set("volatile.eth0.host_name", "vethCCCC")
	before := desired()
	set("volatile.eth1.host_name", "vethBBBB")
	if desired() != before {
		t.Fatal("restart output of a retained NIC changed the pinned Desired")
	}
	set("volatile.eth1.last_state.created", "true")
	if desired() != before {
		t.Fatal("last_state of a retained NIC changed the pinned Desired")
	}
	set("volatile.eth0.host_name", "vethDDDD")
	if desired() != before {
		t.Fatal("eth0 lifecycle control changed the pinned Desired")
	}
	set("volatile.eth1.hwaddr", "00:16:3e:00:00:01")
	if desired() == before {
		t.Fatal("stable device identity left the pinned Desired")
	}
}

func TestRegisteredEndpointKeepsOperatorSettingAndPublishesReceipt(t *testing.T) {
	explicit := []config.SettingResolution{{Scope: "yard", Role: "local settings", Status: "effective"}}
	saved := []config.SettingResolution{{Scope: "derived", Role: "derived", Status: "effective", Detail: "saved endpoint allocation for sample.server"}}
	for name, test := range map[string]struct {
		host, port string
		trace      []config.SettingResolution
	}{
		"explicit-hostname":  {host: "fixture-host.example.test", port: "32000", trace: explicit},
		"saved-hostname":     {host: "fixture-host.example.test", port: "32000", trace: saved},
		"explicit-localhost": {host: "localhost", port: "32000", trace: explicit},
		"saved-localhost":    {host: "localhost", port: "32000", trace: saved},
		"explicit-different": {host: "fixture-other.example.test", port: "45000", trace: explicit},
		"saved-different":    {host: "fixture-other.example.test", port: "45000", trace: saved},
	} {
		t.Run(name, func(t *testing.T) {
			owner, native, path := registeredMigrationFixture(t)
			content := fmt.Sprintf("ENVIRONMENT_PROFILES=sample\nSAMPLE_HOST=%s\nSAMPLE_PORT=%s\n", test.host, test.port)
			testkit.WriteFile(t, path, []byte(content), 0600)
			original := owner.LoadTargets
			owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
				targets, err := original(ctx, view)
				for index := range targets {
					loaded := targets[index].Loaded
					loaded.Environment = maps.Clone(loaded.Environment)
					loaded.Settings = maps.Clone(loaded.Settings)
					loaded.Environment["SAMPLE_HOST"], loaded.Environment["SAMPLE_PORT"] = test.host, test.port
					loaded.Settings["SAMPLE_HOST"] = config.SettingTrace{EffectiveValue: test.host, Resolutions: test.trace}
					loaded.Settings["SAMPLE_PORT"] = config.SettingTrace{EffectiveValue: test.port, Resolutions: test.trace}
					targets[index].Loaded = loaded
				}
				return targets, err
			}
			instance := native.Instances["fixture/yard"]
			retained := map[string]string{"type": "unix-char", "source": "/dev/fixture", "path": "/dev/fixture"}
			instance.LocalDevices["operator-char"], instance.Devices["operator-char"] = retained, maps.Clone(retained)
			beforeDevices := fmt.Sprint(instance.LocalDevices)
			initial, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if err != nil || initial.Converged {
				t.Fatalf("initial: %#v %v", initial, err)
			}
			prepared, present, err := owner.ReadPreparedInput(path)
			if err != nil || !present || string(prepared.Content) != content {
				t.Fatalf("operator setting prepared for rewrite: %q %v", prepared.Content, err)
			}
			if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
				t.Fatal(err)
			}
			final, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if err != nil || !final.Converged || final.Desired != initial.Desired || native.grants != 1 {
				t.Fatalf("final: %#v grants=%d %v", final, native.grants, err)
			}
			written, err := os.ReadFile(path)
			current := native.Instances["fixture/yard"]
			if err != nil || string(written) != content || current.Config["volatile.uuid"] != "uuid-original" || fmt.Sprint(current.LocalDevices) != beforeDevices {
				t.Fatalf("migration changed retained state: %q %v", written, err)
			}
			if current.LocalConfig["user.subyard.resource.sample-server"] == "" {
				t.Fatal("receipt was not published for the retained native device")
			}
		})
	}
}

func TestRegisteredEndpointImportsOverShippedDefault(t *testing.T) {
	owner, _, path := registeredMigrationFixture(t)
	if _, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
	prepared, present, err := owner.ReadPreparedInput(path)
	if err != nil || !present || !strings.Contains(string(prepared.Content), "SAMPLE_PORT='32000'") {
		t.Fatalf("shipped default not imported: %q %v", prepared.Content, err)
	}
}

func TestRegisteredMigrationCollectsResourceAndOwnerRefusals(t *testing.T) {
	owner, native, _ := registeredMigrationFixture(t)
	instance := native.Instances["fixture/yard"]
	key := "user.subyard.resource.sample-server"
	instance.LocalConfig[key], instance.Config[key] = "foreign", "foreign"
	native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
	native.Reconcile.ProjectConfig["user.subyard.operator_uid"] = "foreign"
	want := `yard default: registered project owner differs from protected operator identity; registered resource "sample-server": has foreign or inherited receipt`
	for range 10 {
		_, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
		if err == nil || err.Error() != want {
			t.Fatalf("got %v, want %s", err, want)
		}
	}
	if native.grants != 0 {
		t.Fatal("refused observation published native grants")
	}
}

func TestRegisteredNativeLimitProjectionPreservesCallerEnvironment(t *testing.T) {
	owner, native, _ := registeredMigrationFixture(t)
	targets, err := owner.LoadTargets(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := targets[0].Runtime
	runtime.RepositoryRoot = "../.."
	runtime.Environment = []string{"LIMITS_CPU=8"}
	native.Reconcile.ProjectConfig = map[string]string{}
	native.Reconcile.Instance.Config["limits.cpu"] = "2"
	_, _, _, _, _, _ = inspectRegisteredYardMigration(context.Background(), runtime, nil)
	if runtime.Environment[0] != "LIMITS_CPU=8" {
		t.Fatal("observation changed caller environment", runtime.Environment)
	}
}

func TestRegisteredMigrationDoesNotMistakeMissingSocketForMissingInstance(t *testing.T) {
	for _, missingInstance := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-instance-%t", missingInstance), func(t *testing.T) {
			owner, native, _ := registeredMigrationFixture(t)
			cause := error(&os.PathError{Op: "dial", Path: "/private/socket", Err: os.ErrNotExist})
			if missingInstance {
				cause = ports.ErrInstanceNotFound
			}
			native.Err = cause
			observed, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			if missingInstance {
				if err != nil || !observed.Converged {
					t.Fatal("known absent instance did not skip", err)
				}
			} else if !errors.Is(err, cause) {
				t.Fatal("native socket read failure was treated as absence", err)
			}
			if native.grants != 0 {
				t.Fatal("failed inspection published native grants")
			}
		})
	}
}
