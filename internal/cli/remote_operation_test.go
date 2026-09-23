package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestExactRPCPlanBindingAndSingleUse(t *testing.T) {
	for _, kind := range []string{"success", "missing-digest", "tampered", "expired", "changed-state", "other-session", "stopped"} {
		t.Run(kind, func(t *testing.T) {
			cli, incus, runtime, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
			loaded, err := cli.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			handler := &rpcHandler{cli: cli, loaded: loaded}
			defer handler.closePlans()
			value, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "exact-test", Params: json.RawMessage(`{"command":"integration","arguments":["enable","codex"],"exact":true}`)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan := value.(exactOperationPlan)
			if plan.Schema != 1 || len(plan.Digest) != 64 || !plan.ExpiresAt.After(time.Now()) {
				t.Fatalf("invalid binding: %#v", plan)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			digest := plan.Digest
			switch kind {
			case "missing-digest":
				digest = ""
			case "tampered":
				digest = strings.Repeat("0", 64)
			case "expired":
				plan.ExpiresAt = time.Now().Add(-time.Second)
				handler.exactPlans["exact-test"] = plan
			case "changed-state":
				handler.plans["exact-test"].exactState = "changed"
			case "other-session":
				handler = &rpcHandler{cli: cli, loaded: loaded}
				defer handler.closePlans()
			case "stopped":
				instance := incus.Instances["subyard/yard"]
				instance.Status = "Stopped"
				incus.Instances["subyard/yard"] = instance
			}
			params, _ := json.Marshal(map[string]any{"confirmed": true, "digest": digest})
			_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "exact-test", Params: params}, nil)
			if kind == "success" {
				if err != nil || runtime.applied != 1 {
					t.Fatalf("execute: %v applies=%d", err, runtime.applied)
				}
			} else {
				if err == nil || runtime.applied != 0 {
					t.Fatalf("unsafe execution: %v applies=%d", err, runtime.applied)
				}
				after, _ := os.ReadFile(path)
				if string(after) != string(before) {
					t.Fatal("failed binding changed desired settings")
				}
			}
			_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "exact-test", Params: params}, nil)
			var fault *rpc.Error
			if !errors.As(err, &fault) || fault.Code != "plan_not_found" {
				t.Fatalf("plan reusable after attempt: %v", err)
			}
		})
	}
}

func TestExactRPCRequiresCapabilityAndDiscardsOnDisconnect(t *testing.T) {
	cli, _, runtime, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: cli, loaded: loaded}
	_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "legacy", Params: json.RawMessage(`{"command":"integration","arguments":["enable","codex"]}`)}, nil)
	var fault *rpc.Error
	if !errors.As(err, &fault) || fault.Code != "exact_plan_required" {
		t.Fatalf("legacy plan accepted: %v", err)
	}
	before, _ := os.ReadFile(path)
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		err := (rpc.Session{Handler: handler, Capabilities: []string{exactPlanCapability}}).Serve(context.Background(), server, server)
		handler.closePlans()
		done <- err
	}()
	session := ownerRPCSession{codec: rpc.NewCodec(client, client), close: client.Close}
	if err = session.negotiate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var plan exactOperationPlan
	if err = session.call(context.Background(), "operation.plan", "disconnect", map[string]any{"command": "integration", "arguments": []string{"enable", "codex"}, "exact": true}, &plan); err != nil {
		t.Fatal(err)
	}
	_ = session.close()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session leaked after disconnect")
	}
	after, _ := os.ReadFile(path)
	if len(handler.plans) != 0 || len(handler.exactPlans) != 0 || runtime.applied != 0 || string(before) != string(after) {
		t.Fatal("disconnect retained plan or mutated settings")
	}
}

func TestRemoteIntegrationUsesOneOwnerRPCSession(t *testing.T) {
	for _, kind := range []string{"accept", "ssh-trust-denied", "owner-clock-ahead", "owner-clock-behind", "no-op", "preconfirmed-prompt", "decline", "old-owner", "disconnect", "status", "cleanup-check", "rpc-status", "rpc-status-wrong-yard"} {
		t.Run(kind, func(t *testing.T) {
			selection := "CODING_TOOL_INTEGRATIONS=\n"
			if kind == "no-op" {
				selection = "CODING_TOOL_INTEGRATIONS='codex'\n"
			}
			cli, _, runtime, _, output := integrationFixture(t, selection)
			if kind == "no-op" {
				runtime.plan.Changed = false
				runtime.plan.Steps = nil
			}
			ownerArgs := []string{"enable", "codex"}
			if kind == "cleanup-check" {
				cli, _, _, output = cleanupCLIFixture(t, selection)
				ownerArgs = []string{"cleanup", "codex"}
			}
			owner, err := prepareIntegrationTest(t, cli, ownerArgs...)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "no-op" && (!owner.Plan.Confirmed || owner.Plan.Confirmation != domain.ConfirmationNever || !operationPlanNoOp(owner.Plan)) {
				t.Fatalf("fixture did not produce a native no-op plan: %#v", owner.Plan)
			}
			if kind == "preconfirmed-prompt" {
				owner.Plan.Confirmed = true
			}
			ownerNow := time.Now()
			if kind == "owner-clock-ahead" {
				ownerNow = ownerNow.Add(10 * time.Minute)
			} else if kind == "owner-clock-behind" {
				ownerNow = ownerNow.Add(-10 * time.Minute)
			}
			envelope := bindExactOperationPlan(owner, ownerNow)
			fixture, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			folder := t.TempDir()
			writeCLIFile(t, filepath.Join(folder, "plan.json"), string(fixture), 0600)
			writeCLIFile(t, filepath.Join(folder, "ssh"), `#!/usr/bin/python3
import json,os,struct,sys
root=os.path.dirname(sys.argv[0])
kind=os.environ['EXACT_RPC_KIND']
with open(root+'/calls','a') as out: out.write('session '+repr(sys.argv[1:])+'\n')
def send(req,result):
 data=json.dumps({'version':1,'type':'response','id':req['id'],'operationId':req.get('operationId',''),'result':result}).encode()
 sys.stdout.buffer.write(struct.pack('>I',len(data))+data);sys.stdout.buffer.flush()
while True:
 header=sys.stdin.buffer.read(4)
 if not header: break
 req=json.loads(sys.stdin.buffer.read(struct.unpack('>I',header)[0]))
 if 'deadline' in req:sys.exit(8)
 method=req['method']
 with open(root+'/calls','a') as out: out.write(method+'\n')
 if method=='rpc.negotiate':send(req,{'capabilities':[] if kind=='old-owner' else ['operation-exact-plan-v1']})
 elif method=='operation.plan':
  with open(root+'/plan.json') as source:plan=json.load(source)
  if req['params']!={'command':'integration','arguments':['cleanup' if kind=='cleanup-check' else 'enable','codex'],'exact':True}:sys.exit(4)
  send(req,plan)
  if kind=='disconnect':sys.exit(0)
 elif method=='operation.execute':
  if req['params']!={'confirmed':True,'digest':plan['digest']}:sys.exit(5)
  send(req,{'plan':plan['plan'],'result':{'schema':1,'operationId':req['operationId'],'status':'ok'}})
 elif method=='integration.status':
  if not req['params'].get('ownerOnly'):sys.exit(7)
  send(req,{'yard':'other' if kind=='rpc-status-wrong-yard' else 'default','selection':{'present':True},'observed':'stopped'})
 else:sys.exit(6)
`, 0700)
			t.Setenv("PATH", folder+":"+os.Getenv("PATH"))
			cli.env["PATH"] = os.Getenv("PATH")
			cli.env["EXACT_RPC_KIND"] = kind
			prompt := &testkit.Prompt{Answers: []bool{kind != "decline"}}
			cli.options.Prompt = prompt
			loaded := owner.Loaded
			loaded.Context.AccessKind = domain.AccessRemote
			loaded.Context.OwnerEndpoint = "synthetic-owner"
			loaded.Context.OwnerYardName = "default"
			if strings.HasPrefix(kind, "rpc-status") {
				handler := &rpcHandler{cli: cli, loaded: loaded}
				beforeOperation := cli.env["SUBYARD_OPERATION_ID"]
				result, err := handler.Handle(context.Background(), rpc.Call{
					Method: "integration.status", OperationID: "controller-status", Params: json.RawMessage(`{}`),
				}, nil)
				if kind == "rpc-status-wrong-yard" {
					if err == nil || !strings.Contains(err.Error(), "invalid integration status") {
						t.Fatalf("mismatched owner status accepted: %v", err)
					}
				} else if err != nil || result.(integrationStatus).Observed != "stopped" {
					t.Fatalf("RPC used controller status instead of owner: result=%#v err=%v", result, err)
				}
				if cli.env["SUBYARD_OPERATION_ID"] != beforeOperation || runtime.applied != 0 || len(prompt.Requests) != 0 {
					t.Fatal("RPC status changed controller state or prompted")
				}
				calls, err := os.ReadFile(filepath.Join(folder, "calls"))
				if err != nil || strings.Count(string(calls), "session ") != 1 || !strings.Contains(string(calls), "integration.status") || strings.Contains(string(calls), "operation.plan") {
					t.Fatalf("RPC status did not use one read-only session: %s err=%v", calls, err)
				}
				_, err = handler.Handle(context.Background(), rpc.Call{
					Method: "integration.status", Params: json.RawMessage(`{"ownerOnly":true}`),
				}, nil)
				var fault *rpc.Error
				if !errors.As(err, &fault) || fault.Code != "remote_owner_required" {
					t.Fatalf("remote status allowed recursive owner forwarding: %v", err)
				}
				return
			}
			args := ownerArgs
			if kind == "cleanup-check" {
				args = append(args, "--check")
			}
			if kind == "status" {
				args = []string{"status"}
			}
			trustCalls := 0
			ctx := transport.WithSSHTrust(context.Background(), func(_ context.Context, program, target string) ([]string, error) {
				trustCalls++
				if program != "ssh" || target != "synthetic-owner" {
					t.Fatalf("wrong trust target: %s %s", program, target)
				}
				if kind == "ssh-trust-denied" {
					return nil, domain.ErrOperationDeclined
				}
				return []string{"-o", "HostKeyAlias=reviewed-owner"}, nil
			})
			prepared, err := cli.prepareCommand(ctx, prepareCommandRequest{Loaded: loaded, Definition: owner.Definition, Arguments: args, ReadOnly: kind == "status" || kind == "cleanup-check"})
			if trustCalls != 1 {
				t.Fatalf("SSH trust checks: %d", trustCalls)
			}
			if kind == "ssh-trust-denied" {
				if !errors.Is(err, domain.ErrOperationDeclined) {
					t.Fatalf("trust refusal lost: %v", err)
				}
				if _, err := os.Stat(filepath.Join(folder, "calls")); !os.IsNotExist(err) {
					t.Fatal("owner RPC started before SSH trust")
				}
				return
			}
			if kind == "old-owner" || kind == "preconfirmed-prompt" {
				if err == nil {
					t.Fatalf("unsafe owner plan accepted: %s", kind)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			code := cli.runPreparedCommand(context.Background(), prepared, false)
			want := 0
			if kind == "decline" || kind == "disconnect" {
				want = 1
			}
			if code != want {
				t.Fatalf("exit=%d want=%d output=%s", code, want, output.String())
			}
			calls, err := os.ReadFile(filepath.Join(folder, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(calls), "session ") != 1 || strings.Contains(string(calls), "'enable'") {
				t.Fatalf("raw replay or multiple sessions: %s", calls)
			}
			if !strings.Contains(string(calls), "HostKeyAlias=reviewed-owner") {
				t.Fatal("owner RPC lost verified SSH options")
			}
			wantPrompts := 1
			if kind == "status" || kind == "no-op" || kind == "cleanup-check" {
				wantPrompts = 0
			}
			if len(prompt.Requests) != wantPrompts || runtime.applied != 0 {
				t.Fatalf("controller prompts=%d local applies=%d", len(prompt.Requests), runtime.applied)
			}
			if (kind == "decline" || kind == "cleanup-check") && strings.Contains(string(calls), "operation.execute") {
				t.Fatal("declined plan executed")
			}
			if kind == "no-op" && !strings.Contains(string(calls), "operation.execute") {
				t.Fatal("no-op skipped the owner live recheck")
			}
			if kind == "status" && strings.Contains(string(calls), "operation.plan") {
				t.Fatal("status planned a mutation")
			}
		})
	}
}
