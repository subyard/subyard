package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/hosterrors"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Setup history keeps typed refusal codes and validated names only: a cause's
// text, values and paths never enter it.
func TestSetupOutcomeKeepsCodesAndNamesWithoutValues(t *testing.T) {
	setting, _ := domain.ConfigSettingName("HOST_LINKS")
	profile, _ := domain.ConfigProfileName("route-client")
	cause := fmt.Errorf("prepare: %w", domain.CollectRefusals(
		domain.NewConfigRefusal(domain.ConfigRoleForbidsSetting, setting),
		domain.WithRefusalCause(domain.NewConfigRefusal(domain.ConfigProfileRequiresHostAccess, profile),
			errors.New("raw "+refusalSentinel+" /home/operator/private")),
		domain.SettingRefusal(domain.ConfigSettingNumeric, refusalSentinel),
		errors.New("unclassified "+refusalSentinel),
	))
	outcome := setupOutcome(context.Background(), domain.AdapterResult{}, cause)
	payload, _ := json.Marshal(outcome)
	if strings.Contains(string(payload), refusalSentinel) || strings.Contains(string(payload), "/home/operator") {
		t.Fatalf("outcome leaks a value: %s", payload)
	}
	got := []string{}
	for _, refusal := range outcome.Refusals {
		got = append(got, refusal.Source+":"+refusal.Code+":"+strings.Join(refusal.Names, ","))
	}
	slices.Sort(got)
	if outcome.Status != "failure" || outcome.ErrorCode != "execution_failed" || !outcome.Incomplete ||
		strings.Join(got, " ") != "config:profile-requires-host-access:route-client config:role-forbids-setting:HOST_LINKS" {
		t.Fatalf("outcome=%#v", outcome)
	}
	for _, test := range []struct {
		ctx    context.Context
		result domain.AdapterResult
		err    error
		status string
		code   string
	}{
		{context.Background(), domain.AdapterResult{Status: "ok"}, nil, "success", ""},
		{context.Background(), domain.AdapterResult{Status: "error", ErrorCode: "provision_failed"}, nil, "failure", "provision_failed"},
		{context.Background(), domain.AdapterResult{Status: "error", ErrorCode: "Free text"}, nil, "failure", "execution_failed"},
		{context.Background(), domain.AdapterResult{}, fmt.Errorf("x: %w", domain.ErrPlanStale), "failure", "plan_stale"},
		{context.Background(), domain.AdapterResult{}, &application.ProjectHookFailure{}, "failure", "project_hook_failed"},
		{context.Background(), domain.AdapterResult{}, context.Canceled, "interrupted", "context_cancelled"},
	} {
		outcome := setupOutcome(test.ctx, test.result, test.err)
		if outcome.Status != test.status || outcome.ErrorCode != test.code {
			t.Errorf("%v %v: outcome=%#v", test.result, test.err, outcome)
		}
	}
}

// An init refused by its configuration leaves a terminal setup attempt.
func TestInitPreparationFailureIsRecordedInSetupHistory(t *testing.T) {
	fixture := newFixture(t)
	configHome := environmentValue(fixture.Environment, "SUBYARD_CONFIG_HOME")
	home := environmentValue(fixture.Environment, "SUBYARD_HOME")
	writeConfigCommandFile(t, filepath.Join(configHome, "yards/broken/config.env"), "SSH_PORT=sentinel\n")
	var output bytes.Buffer
	program := fixture.New(Options{Arguments: []string{"-Y", "broken", "init", "--yes"}, Stdout: &output, Stderr: &output})
	if code := program.Run(context.Background()); code == 0 {
		t.Fatalf("init succeeded: %s", output.String())
	}
	records, err := (audit.SetupHistory{Home: home}).Read(10)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%#v err=%v output=%s", records, err, output.String())
	}
	record := records[0]
	if payload, _ := json.Marshal(record); strings.Contains(string(payload), "sentinel") {
		t.Fatalf("setup history leaks the value: %s", payload)
	}
	if record.Yard != "broken" || record.Status != "failure" || record.Phase != "prepare" || record.ErrorCode != "configuration_invalid" ||
		!record.Incomplete || len(record.Refusals) != 1 || record.Refusals[0].Code != "source-invalid" ||
		strings.Join(record.Refusals[0].Names, ",") != "SSH_PORT" {
		t.Fatalf("record=%#v", record)
	}
}

func TestPublishHostErrorsWritesTheSnapshotIntoTheRunningYard(t *testing.T) {
	fixture := newFixture(t)
	home := environmentValue(fixture.Environment, "SUBYARD_HOME")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(fixture.Root, hostErrorsProgram)), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(fixture.Root, hostErrorsProgram), []byte("# facts helper\n"), 0o644)
	if err := (audit.RPCErrorLog{Home: home}).Append(context.Background(), audit.RPCError{
		At: time.Now(), Yard: "work", Request: "r1", Method: rpc.MethodOperationPlan, Code: string(rpc.CodePlanFailed), Error: refusalSentinel,
	}); err != nil {
		t.Fatal(err)
	}
	yard := domain.Context{YardName: "work", IncusProject: "subyard-work", YardInstanceName: "yard", AccessKind: domain.AccessLocal}
	incus := &testkit.Incus{
		Instances: map[string]ports.InstanceInfo{"subyard-work/yard": {Name: "yard", Project: "subyard-work", Status: "Running"}},
		ExecSteps: []testkit.IncusExecStep{{}},
	}
	program := fixture.New(Options{Incus: incus, Executor: incus})
	program.publishHostErrors(context.Background(), yard)
	if len(incus.ExecCalls) != 1 {
		t.Fatalf("exec calls=%#v", incus.ExecCalls)
	}
	call := incus.ExecCalls[0]
	if call.Project != "subyard-work" || call.Name != "yard" || len(call.Request.Command) != 5 ||
		call.Request.Command[3] != "# facts helper\n" || call.Request.Command[4] != "host-errors-put" {
		t.Fatalf("call=%#v", call)
	}
	if strings.Contains(string(call.Request.Stdin), refusalSentinel) {
		t.Fatalf("snapshot leaks error text: %s", call.Request.Stdin)
	}
	var snapshot hosterrors.Snapshot
	if err := json.Unmarshal(call.Request.Stdin, &snapshot); err != nil || snapshot.Yard != "work" || len(snapshot.RPC) != 1 ||
		snapshot.RPC[0].Code != string(rpc.CodePlanFailed) {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	// A stopped yard, a remote yard and a failing exec change nothing.
	incus.Instances["subyard-work/yard"] = ports.InstanceInfo{Name: "yard", Project: "subyard-work", Status: "Stopped"}
	program.publishHostErrors(context.Background(), yard)
	remote := yard
	remote.AccessKind = domain.AccessRemote
	program.publishHostErrors(context.Background(), remote)
	if len(incus.ExecCalls) != 1 {
		t.Fatalf("exec calls=%#v", incus.ExecCalls)
	}
}

// Failures carry the session's yard, and the writer refreshes the snapshot
// after failures and executed operations, and once more at the end.
func TestRPCRecorderKeepsSessionYardAndRefreshesTheSnapshot(t *testing.T) {
	home := testkit.TempDir(t)
	published := make(chan struct{}, 8)
	recorder, closeRecorder := startRPCErrorRecorder(rpc.HandlerFunc(
		func(_ context.Context, call rpc.Call, _ rpc.Emit) (any, error) {
			if call.Method == rpc.MethodOperationPlan {
				return nil, errors.New("failed")
			}
			return nil, nil
		}), audit.RPCErrorLog{Home: home}, "work", func(context.Context) { published <- struct{}{} })
	if _, err := recorder.Handle(context.Background(), rpc.Call{ID: "r1", Method: rpc.MethodOperationPlan}, nil); err == nil {
		t.Fatal("the recorder hid the failure")
	}
	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("a failure did not refresh the snapshot")
	}
	_, _ = recorder.Handle(context.Background(), rpc.Call{ID: "r2", Method: rpc.MethodContextGet}, nil)
	_, _ = recorder.Handle(context.Background(), rpc.Call{ID: "r3", Method: rpc.MethodOperationExecute}, nil)
	closeRecorder()
	if len(published) != 1 {
		t.Fatalf("published %d more times, want one final refresh after the executed operation", len(published))
	}
	records, err := (audit.RPCErrorLog{Home: home}).Read(10)
	if err != nil || len(records) != 1 || records[0].Yard != "work" {
		t.Fatalf("records=%#v err=%v", records, err)
	}
}
