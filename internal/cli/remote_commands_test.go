package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Subyard/Subyard/internal/adapters/remotecontrol"
	"github.com/Subyard/Subyard/internal/audit"
	"github.com/Subyard/Subyard/internal/ports"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

type remoteControlStub struct {
	applied int
	records []domain.RemoteRecord
}

func (control *remoteControlStub) Lookup(context.Context, string) (domain.RemoteRecord, bool, error) {
	return domain.RemoteRecord{}, false, nil
}
func (control *remoteControlStub) List(context.Context) ([]domain.RemoteRecord, error) {
	return control.records, nil
}
func (control *remoteControlStub) ProbeOwner(context.Context, domain.RemoteSpec) (domain.RemoteInfo, error) {
	return domain.RemoteInfo{State: "RUNNING", SSHPort: 2222, DevUser: "dev"}, nil
}
func (control *remoteControlStub) ScanYardKeys(context.Context, domain.RemoteSpec, int) ([]domain.RemoteKey, error) {
	return []domain.RemoteKey{{Material: "ssh-ed25519 fixture", Fingerprint: "SHA256:new"}}, nil
}
func (control *remoteControlStub) RecordedYardKeys(context.Context, string) ([]domain.RemoteKey, error) {
	return nil, nil
}
func (control *remoteControlStub) Apply(_ context.Context, prepared domain.RemotePrepared) (domain.RemoteResult, error) {
	control.applied++
	return domain.RemoteResult{Message: string(prepared.Action)}, nil
}

func TestRPCRemotePlanContainsPreparedFingerprintBeforeApply(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	control := &remoteControlStub{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, RemoteControl: control,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	params, _ := json.Marshal(map[string]any{
		"command": "remote", "arguments": []string{"add", "demo", "owner"},
	})
	result, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "operation-remote", Params: params,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := result.(domain.OperationPlan)
	if control.applied != 0 || !strings.Contains(strings.Join(plan.Consequences, " "), "SHA256:new") {
		t.Fatalf("remote prepare mutated state or omitted fingerprint: %#v", plan)
	}
	execute, _ := json.Marshal(map[string]bool{"confirmed": true})
	if _, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.execute", OperationID: plan.OperationID, Params: execute,
	}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	if control.applied != 1 {
		t.Fatalf("confirmed plan applied %d times", control.applied)
	}
}

func TestDirectAndRPCRemoteAddUseEquivalentOwnerActionPlans(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	directControl := &remoteControlStub{}
	directPrompt := &testkit.Prompt{Answers: []bool{true}}
	direct, err := New(Options{
		RepositoryRoot: root, Program: "yard", Arguments: []string{"remote", "add", "demo", "owner"},
		Environment: append(environment, "SUBYARD_OPERATION_ID=operation-remote-direct"), RemoteControl: directControl,
		Prompt: directPrompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := direct.Run(context.Background()); code != 0 || directControl.applied != 1 || len(directPrompt.Requests) != 1 {
		t.Fatalf("direct code=%d applies=%d requests=%#v", code, directControl.applied, directPrompt.Requests)
	}

	rpcControl := &remoteControlStub{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, RemoteControl: rpcControl,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	params, _ := json.Marshal(map[string]any{
		"command": "remote", "arguments": []string{"add", "demo", "owner"},
	})
	result, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "operation-remote-rpc", Params: params,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := result.(domain.OperationPlan)
	if rpcControl.applied != 0 || plan.Assessment == nil || plan.Assessment.Action != "remote.add" ||
		plan.Confirmation != domain.ConfirmationPromptDefaultYes || plan.ConfirmationRequest == nil ||
		!reflect.DeepEqual(*plan.ConfirmationRequest, directPrompt.Requests[0]) {
		t.Fatalf("direct request=%#v rpc plan=%#v applies=%d", directPrompt.Requests[0], plan, rpcControl.applied)
	}
	execute, _ := json.Marshal(map[string]bool{"confirmed": true})
	if _, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.execute", OperationID: plan.OperationID, Params: execute,
	}, func(string, any) (uint64, error) { return 1, nil }); err != nil || rpcControl.applied != 1 {
		t.Fatalf("execute err=%v applies=%d", err, rpcControl.applied)
	}
}

func TestRPCOperationPlanMapsActionPolicyInvalid(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, RemoteControl: &remoteControlStub{},
	})
	if err != nil {
		t.Fatal(err)
	}
	program.coreActions, err = domain.NewActionRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	params, _ := json.Marshal(map[string]any{
		"command": "remote", "arguments": []string{"add", "demo", "owner"},
	})
	_, err = handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "operation-policy-plan", Params: params,
	}, nil)
	rpcErr, ok := err.(*rpc.Error)
	if !ok || rpcErr.Code != domain.ActionPolicyInvalid {
		t.Fatalf("operation.plan err=%#v", err)
	}
}

func TestRPCOperationExecuteRejectsForgedPlanConfirmation(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	control := &remoteControlStub{}
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, RemoteControl: control,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	params, _ := json.Marshal(map[string]any{
		"command": "remote", "arguments": []string{"add", "demo", "owner"},
	})
	result, err := handler.Handle(context.Background(), rpc.Call{
		Method: "operation.plan", OperationID: "operation-policy-execute", Params: params,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := result.(domain.OperationPlan)
	handler.plansMu.Lock()
	planned := handler.plans[plan.OperationID]
	planned.Plan.Confirmed = true
	handler.plans[plan.OperationID] = planned
	handler.plansMu.Unlock()
	execute, _ := json.Marshal(map[string]bool{"confirmed": true})
	_, err = handler.Handle(context.Background(), rpc.Call{
		Method: "operation.execute", OperationID: plan.OperationID, Params: execute,
	}, func(string, any) (uint64, error) { return 1, nil })
	rpcErr, ok := err.(*rpc.Error)
	if !ok || rpcErr.Code != "plan_stale" || control.applied != 0 {
		t.Fatalf("operation.execute err=%#v applies=%d", err, control.applied)
	}
}

// The SSH command boundary runs another actual CLI process: owner locks and
// auditing are native; Incus and the SSH wire remain fixture boundaries.
func TestRemoteAddAuditedOwnerChild(t *testing.T) {
	if os.Getenv("REMOTE_OWNER_CHILD") == "1" {
		var input struct {
			Root        string
			Environment []string
		}
		body, err := os.ReadFile(os.Getenv("REMOTE_OWNER_INPUT"))
		if err != nil || json.Unmarshal(body, &input) != nil {
			os.Exit(90)
		}
		native := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {Status: "Running"}}, ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{Stdout: []byte("added"), ExitCode: 0}}}}
		var stderr bytes.Buffer
		program, err := New(Options{RepositoryRoot: input.Root, Program: "yard", Arguments: []string{"_authorize"}, Environment: input.Environment, WorkingDir: input.Root, Stdin: os.Stdin, Stderr: &stderr, Incus: native, Executor: native})
		if err != nil {
			os.Exit(91)
		}
		if os.Getenv("REMOTE_OWNER_SLOW_ADMISSION") == "1" {
			time.Sleep(11 * time.Second)
		}
		code := program.Run(context.Background())
		if code == 0 && len(native.ExecCalls) != 1 {
			os.Exit(92)
		}
		os.Exit(code)
	}
	options, runtime := auditedRemoteOwnerFixture(t)
	var stdout, stderr bytes.Buffer
	options.Stdout = &stdout
	options.Stderr = &stderr
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if code := program.Run(ctx); code != 0 {
		t.Fatalf("actual native remote add failed code=%d: %s", code, stderr.String())
	}
	if _, exists, err := runtime.Lookup(context.Background(), "self-fixture"); err != nil || !exists {
		t.Fatal(fmt.Sprintf("registration missing: %v", err))
	}
}

func auditedRemoteOwnerFixture(t *testing.T) (Options, remotecontrol.Runtime) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	filtered := []string{}
	for _, value := range environment {
		if !strings.HasPrefix(value, "SUBYARD_NO_AUDIT=") {
			filtered = append(filtered, value)
		}
	}
	environment = filtered
	nativeInstallationFixture(t, environment)
	for _, name := range []string{"HOME", "SUBYARD_CONFIG_HOME"} {
		if err := os.MkdirAll(environmentValue(environment, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	bin, trust := hostAddSSHFixture(t, inventoryResult("fixture-owner", "default", "").inventory)
	public := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA fixture\n"
	key := filepath.Join(root, "controller.pub")
	testkit.WriteFile(t, key, []byte(public), 0600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "child-input.json")
	body, _ := json.Marshal(struct {
		Root        string
		Environment []string
	}{root, environment})
	testkit.WriteFile(t, input, body, 0600)
	sshPath := filepath.Join(bin, "ssh")
	script, err := os.ReadFile(sshPath)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), "cat \"$SSH_RPC_RESPONSE\"", `case "$*" in
 *_authorize*)
 if [ -n "${REMOTE_GAP-}" ]; then
  : > "$REMOTE_GAP/entered"
  count=0
  while [ ! -e "$REMOTE_GAP/release" ] && [ "$count" -lt 100 ]; do count=$((count+1)); sleep 0.02; done
  [ -e "$REMOTE_GAP/release" ]; exit $?
 fi
 exec "$REMOTE_OWNER_BINARY" -test.run=^TestRemoteAddAuditedOwnerChild$ ;;
 *ssh-keyscan*) printf '%s\n' "$SSH_ASSESS_LINE"; exit 0 ;;
 *yard-self-fixture*) printf '%s\n' "$REMOTE_YARD_KEY" > "$REMOTE_KNOWN"; chmod 600 "$REMOTE_KNOWN"; exit 0 ;;
esac
cat "$SSH_RPC_RESPONSE"`, 1))
	testkit.WriteFile(t, sshPath, script, 0700)
	home := environmentValue(environment, "HOME")
	data := environmentValue(environment, "SUBYARD_HOME")
	conf := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	environment = append(environment, "PATH="+bin+":"+os.Getenv("PATH"), "SSH_RPC_RESPONSE="+os.Getenv("SSH_RPC_RESPONSE"), "SSH_ASSESS_LINE="+trust.KnownHostsLine, "REMOTE_OWNER_CHILD=1", "REMOTE_OWNER_INPUT="+input, "REMOTE_OWNER_BINARY="+executable, "REMOTE_YARD_KEY="+strings.Replace(trust.KnownHostsLine, "owner-alias", "subyard-remote-self-fixture", 1), "REMOTE_KNOWN="+filepath.Join(data, "ssh", "known_hosts"))
	runtime := remotecontrol.Runtime{SSH: sshPath, Home: home, DataHome: data, ConfigHome: conf, ConfigDir: filepath.Join(root, "config"), PublicKey: key, Environment: environment, Timeout: 2 * time.Second}
	return Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"remote", "add", "self-fixture", "owner-alias", "--yes"}, Environment: environment, WorkingDir: root, RemoteControl: runtime, Prompt: &testkit.Prompt{Answers: []bool{true}}}, runtime
}

func TestRPCRemoteAddRefinesOwnFirstHomePublication(t *testing.T) {
	options, runtime := auditedRemoteOwnerFixture(t)
	if err := os.RemoveAll(runtime.DataHome); err != nil {
		t.Fatal(err)
	}
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	params, _ := json.Marshal(map[string]any{"command": "remote", "arguments": []string{"add", "self-fixture", "owner-alias"}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	result, err := handler.Handle(ctx, rpc.Call{Method: "operation.plan", OperationID: "remote-own-home", Params: params}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(runtime.DataHome); !os.IsNotExist(err) {
		t.Fatal("planning created the absent owner home", err)
	}
	plan := result.(domain.OperationPlan)
	execute, _ := json.Marshal(map[string]bool{"confirmed": true})
	if _, err := handler.Handle(ctx, rpc.Call{Method: "operation.execute", OperationID: plan.OperationID, Params: execute}, func(string, any) (uint64, error) { return 1, nil }); err != nil {
		t.Fatal("own first home publication invalidated original authority", err)
	}
	if _, exists, err := runtime.Lookup(context.Background(), "self-fixture"); err != nil || !exists {
		t.Fatal("registration missing", err)
	}
	if _, err := os.Stat(filepath.Join(runtime.DataHome, "logs")); err != nil {
		t.Fatal("audited child omitted its native audit write", err)
	}
}

func TestRemoteAddAuditedOwnerAfterSlowAdmission(t *testing.T) {
	options, runtime := auditedRemoteOwnerFixture(t)
	runtime.Timeout = 0
	runtime.Environment = append(runtime.Environment, "REMOTE_OWNER_SLOW_ADMISSION=1")
	options.RemoteControl = runtime
	var stderr bytes.Buffer
	options.Stderr = &stderr
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if code := program.Run(ctx); code != 0 {
		t.Fatalf("prepared authorization did not cover slow native admission plus audited child: code=%d %s", code, stderr.String())
	}
	if _, exists, err := runtime.Lookup(context.Background(), "self-fixture"); err != nil || !exists {
		t.Fatal("local registration missing", err)
	}
	lines, err := audit.ReadAuditLines(runtime.DataHome, 50)
	if err != nil {
		t.Fatal(err)
	}
	observed := false
	for _, line := range lines {
		if strings.Contains(line, " -- _authorize") {
			observed = true
		}
	}
	if !observed {
		t.Fatal("successful authorization omitted actual child audit")
	}
}
