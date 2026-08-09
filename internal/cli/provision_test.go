package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProvisionSelectionUsesYardThenProjectProfiles(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "android")
	writeProvisionProfile(t, root, "hermes")
	writeProvisionProfile(t, root, "openclaw")
	writeProvisionProfile(t, root, "subyard-dev")
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Environment["YARD_PROFILES"] = "openclaw android"
	execution, err := program.prepareProvisionExecution(loaded, nil, &projectExecution{
		Environment: map[string]string{"SUBYARD_PROJECT_PROFILES": "android subyard-dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(execution.profiles, []string{"openclaw", "android", "subyard-dev"}) {
		t.Fatalf("profiles=%v", execution.profiles)
	}
	loaded.Environment["YARD_PROFILES"] = "hermes"
	execution, err = program.prepareProvisionExecution(loaded, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(execution.profiles, []string{"hermes"}) {
		t.Fatalf("Hermes no-argument profiles=%v", execution.profiles)
	}
}

func TestProvisionCLIAndRPCUseNativeRunner(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	writeProvisionProfile(t, root, "subyard-dev")
	for _, rpcMode := range []bool{false, true} {
		incus := lifecycleIncus()
		instance := incus.Instances["subyard/yard"]
		instance.Status = "Running"
		incus.Instances["subyard/yard"] = instance
		clock := testkit.NewManualClock(time.Unix(100, 0))
		operationID := "provision-cli"
		if rpcMode {
			operationID = "provision-rpc"
		}
		runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
			Schema: 1, OperationID: operationID, Status: "ok",
		}}}}
		options := Options{
			RepositoryRoot: root, Program: "yard", Environment: environment,
			WorkingDir: root, Incus: incus, AdapterRunner: runner, Clock: clock,
		}
		if !rpcMode {
			options.Arguments = []string{"provision", "--yes"}
			options.Environment = append(slices.Clone(environment), "SUBYARD_OPERATION_ID="+operationID)
			program, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("CLI provision failed with %d", code)
			}
		} else {
			program, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
			if _, err := handler.Handle(context.Background(), rpc.Call{
				ID: "plan", OperationID: operationID, Method: "operation.plan",
				Params: json.RawMessage(`{"command":"provision","arguments":[]}`),
			}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := handler.Handle(context.Background(), rpc.Call{
				ID: "execute", OperationID: operationID, Method: "operation.execute",
				Params: json.RawMessage(`{"confirmed":true}`),
			}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
				t.Fatal(err)
			}
		}
		if len(runner.Requests) != 1 || runner.Requests[0].Action != "profile" ||
			!slices.Equal(runner.Requests[0].Arguments, []string{"subyard-dev"}) {
			t.Fatalf("rpc=%v physical=%v", rpcMode, runner.Requests)
		}
	}
}

func writeProvisionProfile(t *testing.T, root, name string) {
	t.Helper()
	directory := filepath.Join(root, "config", "profiles", name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(directory, "profile.conf"), "PROFILE_NAME="+name+"\n", 0o600)
	writeCLIFile(t, filepath.Join(directory, "provision.sh"), "true\n", 0o700)
}
