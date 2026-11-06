package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAppliedIdentityHintPublishesVerifiedIdentityAfterRecreation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	for _, name := range []string{"transition.py", "subids.py", "accounts.py"} {
		body, err := os.ReadFile(filepath.Join("../../config/identity", name))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "config/identity", name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, body, 0600)
	}
	native := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	cli, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: native, Executor: native})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	old := config.AppliedIdentityHint{Schema: 1, Yard: loaded.Context.YardName, UUID: "11111111-1111-1111-1111-111111111111", Source: strings.Repeat("a", 64), Release: strings.Repeat("b", 64), User: "previous", UID: 1001}
	if err := os.MkdirAll(loaded.Context.Paths.ConfigHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := config.PublishAppliedIdentityHint(loaded.Context.Paths.ConfigHome, old, config.PersistentFileSnapshot{}); err != nil {
		t.Fatal(err)
	}
	prepared := &preparedCommand{CLI: cli, Loaded: loaded}
	if handled, err := prepared.prepareStoppedIdentityInspection(context.Background(), nil); err != nil || handled {
		t.Fatal("missing native instance incorrectly blocked guarded recreation", handled, err)
	}
	key := loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName
	uuid := "22222222-2222-2222-2222-222222222222"
	native.Instances[key] = ports.InstanceInfo{Name: loaded.Context.YardInstanceName, Status: "Running", Config: map[string]string{"volatile.uuid": uuid}}
	observed := []byte(`{"installed":true,"applied_settings":{"DEV_USER":"verified-user","DEV_UID":"1001"},"selected":[]}`)
	native.ExecSteps = []testkit.IncusExecStep{{Result: ports.InstanceExecResult{Stdout: observed}}, {Result: ports.InstanceExecResult{Stdout: observed}}, {Result: ports.InstanceExecResult{Stdout: []byte(`{}`)}}}
	if err := cli.publishAppliedIdentityHint(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	hint, _, err := config.ReadAppliedIdentityHint(loaded.Context.Paths.ConfigHome, loaded.Context.YardName)
	if err != nil || hint == nil || hint.UUID != uuid || hint.User != "verified-user" || hint.UID != 1001 || hint.User == loaded.Context.DevUser {
		t.Fatal("hint did not publish verified applied identity on recreated instance", hint, err)
	}
	if len(native.ExecCalls) != 3 {
		t.Fatal("missing native applied identity verification", len(native.ExecCalls))
	}
	probe := native.ExecCalls[2].Request
	var request map[string]any
	if json.Unmarshal(probe.Stdin, &request) != nil || request["user"] != "verified-user" || probe.Command[len(probe.Command)-1] != "applied" {
		t.Fatal("hint trusted desired settings without native verification")
	}
	if markers, err := config.PendingDeveloperIdentities(loaded.Context.Paths.ConfigHome); err != nil || len(markers) != 0 {
		t.Fatal("non-authorizing hint fenced ordinary mutations", markers, err)
	}
}

// Fresh hosts run init before Incus exists: identity inspection must leave installation to
// ordinary init, while a pending rename still refuses without its native proof.
func TestIdentityInspectionLeavesUnreachableIncusToOrdinaryInit(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	unreachable := errors.New(`connect to Incus: dial unix /var/lib/incus/unix.socket: connect: no such file or directory`)
	native := &testkit.Incus{Err: unreachable}
	cli, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: native, Executor: native})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	prepared := &preparedCommand{CLI: cli, Loaded: loaded}
	check := func(name string, prepare func(context.Context, *initBootstrap) (bool, error), refuses bool) {
		t.Helper()
		handled, err := prepare(context.Background(), nil)
		if handled || refuses != (err != nil) {
			t.Fatalf("%s: handled=%v err=%v, want refusal=%v", name, handled, err, refuses)
		}
	}
	for _, configHome := range []bool{false, true} {
		if configHome {
			if err := os.MkdirAll(loaded.Context.Paths.ConfigHome, 0700); err != nil {
				t.Fatal(err)
			}
		}
		check("stopped inspection", prepared.prepareStoppedIdentityInspection, false)
		check("identity init", prepared.prepareDeveloperIdentityInit, false)
	}
	marker := config.DeveloperIdentityMarkerPath(loaded.Context.Paths.ConfigHome, loaded.Context.YardName)
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, marker, []byte(`{}`), 0600)
	check("pending stopped inspection", prepared.prepareStoppedIdentityInspection, true)
	check("pending identity init", prepared.prepareDeveloperIdentityInit, true)
}

func TestStoppedIdentityInspectionCapturesPrivateApprovalScope(t *testing.T) {
	for _, scenario := range []string{"missing", "match", "mismatch", "foreign", "source", "desired", "uuid", "hint"} {
		t.Run(scenario, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			dispatcher, err := os.ReadFile("../../config/projects-changed.sh")
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, root+"/config/projects-changed.sh", dispatcher, 0755)
			native := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
			cli, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: native, Executor: native})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := cli.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(loaded.Context.Paths.ConfigHome, 0700); err != nil {
				t.Fatal(err)
			}
			key := loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName
			uuid := "11111111-1111-1111-1111-111111111111"
			native.Instances[key] = ports.InstanceInfo{Name: loaded.Context.YardInstanceName, Status: "Stopped", Config: map[string]string{"volatile.uuid": uuid}}
			runtime := cli.initPlatform(loaded, nil).(reconcileruntime.Runtime)
			source, _, err := runtime.DeveloperIdentityObservationBinding(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			release, err := identityReleaseBinding(loaded.Context.Paths.ConfigHome)
			if err != nil {
				t.Fatal(err)
			}
			hint := config.AppliedIdentityHint{Schema: 1, Yard: loaded.Context.YardName, UUID: uuid, Source: source, Release: release, User: loaded.Context.DevUser, UID: loaded.Context.DevUID}
			if scenario != "missing" {
				if scenario != "match" {
					hint.User = "old-developer"
				}
				if scenario == "foreign" {
					hint.UUID = "22222222-2222-2222-2222-222222222222"
				}
				if err := config.PublishAppliedIdentityHint(loaded.Context.Paths.ConfigHome, hint, config.PersistentFileSnapshot{}); err != nil {
					t.Fatal(err)
				}
			}
			prepared := &preparedCommand{CLI: cli, Loaded: loaded}
			handled, err := prepared.prepareStoppedIdentityInspection(context.Background(), nil)
			if scenario == "foreign" {
				if !errors.Is(err, domain.ErrPlanStale) {
					t.Fatal("foreign hint accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "match" {
				if handled {
					t.Fatal("matching hint booted converged stopped yard")
				}
				return
			}
			if !handled {
				t.Fatal("stopped account inspection omitted")
			}
			if err := domain.ValidateOperationSteps(prepared.steps()); err != nil {
				t.Fatal(err)
			}
			if len(native.ExecCalls) != 0 || len(native.ConfigUpdates) != 0 || len(native.PowerUpdates) != 0 {
				t.Fatal("assessment wrote or booted guest")
			}
			switch scenario {
			case "source":
				testkit.WriteFile(t, root+"/config/projects-changed.sh", []byte("changed"), 0600)
			case "desired":
				cli.baseEnv["DEV_USER"] = "foreign"
			case "uuid":
				state := native.Instances[key]
				state.Config = map[string]string{"volatile.uuid": "22222222-2222-2222-2222-222222222222"}
				native.Instances[key] = state
			case "hint":
				_, snapshot, err := config.ReadAppliedIdentityHint(loaded.Context.Paths.ConfigHome, loaded.Context.YardName)
				if err != nil {
					t.Fatal(err)
				}
				hint.Source = strings.Repeat("e", 64)
				if err := config.PublishAppliedIdentityHint(loaded.Context.Paths.ConfigHome, hint, snapshot); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err = prepared.refresh(context.Background())
			if scenario == "missing" || scenario == "mismatch" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("changed inspection scope accepted")
			}
			if len(native.ConfigUpdates) != 0 || len(native.PowerUpdates) != 0 {
				t.Fatal("stale/declined plan changed power")
			}
		})
	}
}

func TestStoppedIdentityInspectionImportsPowerBeforePhysicalStart(t *testing.T) {
	for _, desired := range []string{"running", "stopped", "legacy"} {
		t.Run(desired, func(t *testing.T) {
			yard := domain.Context{YardName: "default", IncusProject: "project", YardInstanceName: "yard", IncusBridge: "incusbr0"}
			values := map[string]string{"volatile.uuid": "11111111-1111-1111-1111-111111111111", "foreign.retained": "exact"}
			wanted := desired
			if desired == "legacy" {
				wanted = "stopped"
			} else {
				for key, value := range map[string]string{"user.subyard.managed": "true", "user.subyard.name": "default", "user.subyard.bridge": "incusbr0", "user.subyard.initialized": "true", "user.subyard.desired_power": desired, "boot.autostart": "false"} {
					values[key] = value
				}
			}
			before := ports.InstanceInfo{Name: "yard", Status: "Stopped", Config: values}
			native := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"project/yard": before}}
			power := application.PowerService{Instances: native, Config: native}
			called := false
			expected, err := startIdentityInspectionPower(context.Background(), power, yard, before, func() error {
				called = true
				current := native.Instances["project/yard"]
				if current.Config["user.subyard.desired_power"] != wanted || current.Config["foreign.retained"] != "exact" {
					t.Fatal("boot imported post-start intent or changed foreign data")
				}
				current.Status = "Running"
				native.Instances["project/yard"] = current
				return nil
			})
			if err != nil || !called {
				t.Fatal(err)
			}
			actual := native.Instances["project/yard"]
			if identityInspectionInstanceBinding(actual) != identityInspectionInstanceBinding(expected) {
				t.Fatal("power import escaped expected native envelope")
			}
			actual.Config["foreign.retained"] = "changed"
			if identityInspectionInstanceBinding(actual) == identityInspectionInstanceBinding(expected) {
				t.Fatal("foreign native change accepted after boot")
			}
		})
	}
}
