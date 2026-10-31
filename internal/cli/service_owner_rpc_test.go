package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOwnerServiceRPCWithInvalidWorkspaceAllowsOnlyBoundServiceOperations(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	configHome := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(configHome, "yards/fixture-runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(configHome, "yards/fixture-runtime/config.env"), []byte("SSH_PORT=3333\nNESTED_E2E_VMS=1\n"), 0o600)
	instance := ports.InstanceInfo{Name: "yard-fixture-runtime", Project: "subyard-fixture-runtime", Status: "Running", Config: map[string]string{"user.subyard.managed": "true", "user.subyard.name": "fixture-runtime", "user.subyard.initialized": "true", "user.subyard.desired_power": "running", "user.subyard.bridge": "incusbr0", "boot.autostart": "false"}}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{instance.Project + "/" + instance.Name: instance}}
	probe := &testVMStatusProbe{output: []byte(`{"schema_version":1,"status":"ok","pool":{"schema_version":2,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":3,"state":"held"}]}}`)}
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{
		{Result: domain.AdapterResult{Schema: 1, OperationID: "service-status", Status: "ok"}, Stderr: string(probe.output)},
		{Result: domain.AdapterResult{Schema: 1, OperationID: "service-revoke", Status: "ok"}, Apply: func(domain.AdapterRequest) {
			probe.output = bytes.Replace(probe.output, []byte(`"state":"held"`), []byte(`"state":"available"`), 1)
		}},
	}}
	client, server := net.Pipe()
	var diagnostics bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Arguments: []string{"rpc", "--stdio"}, Environment: environment, Incus: incus, ProjectData: probe, AdapterRunner: runner, Stdin: server, Stdout: server, Stderr: &diagnostics})
	if err != nil {
		t.Fatal(err)
	}
	selection := infrastructureYardFixture(t, program, incus, "test-vms", "fixture-runtime")
	if err := os.MkdirAll(filepath.Join(configHome, "yards/default"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(configHome, "yards/default/config.env"), []byte("SSH_PORT=invalid\n"), 0o600)
	// Start from original caller inputs, as a new SSH invocation does.
	program.env = program.serviceContextReader().env
	done := make(chan int, 1)
	go func() { done <- program.Run(context.Background()); _ = server.Close() }()
	session := ownerRPCSession{codec: rpc.NewCodec(client, client), close: client.Close}
	defer func() {
		_ = client.Close()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("RPC exit=%d: %s", code, diagnostics.String())
			}
		case <-time.After(3 * time.Second):
			t.Error("RPC did not drain on EOF")
		}
	}()
	if err := session.negotiate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(session.capabilities, ownerServicesCapability) || slices.Contains(session.capabilities, "context") || slices.Contains(session.capabilities, "snapshot") {
		t.Fatalf("unsafe fallback capabilities: %v", session.capabilities)
	}
	for _, method := range []string{"context.get", "snapshot.get", "owner.inventory", "project.list"} {
		var result any
		err := session.call(context.Background(), method, "refusal-"+method, struct{}{}, &result)
		var failure *rpc.Error
		if !errors.As(err, &failure) || failure.Code != "owner_configuration_invalid" || strings.Contains(failure.Message, "SSH_PORT") {
			t.Fatalf("%s: %v", method, err)
		}
	}
	var denied any
	err = session.call(context.Background(), "operation.plan", "nonservice", map[string]any{"command": "start", "arguments": []string{}, "exact": true}, &denied)
	var failure *rpc.Error
	if !errors.As(err, &failure) || failure.Code != "owner_configuration_invalid" {
		t.Fatalf("nonservice mutation: %v", err)
	}
	if len(runner.Requests) != 0 || len(incus.PowerUpdates) != 0 || len(incus.ConfigUpdates) != 0 {
		t.Fatal("fallback refusal performed native effects")
	}
	var plan domain.OperationPlan
	if err := session.call(context.Background(), "operation.plan", "service-status", map[string]any{"command": "test-vms", "arguments": []string{"status", "--json", "--expect-owner", selection.record.Owner}}, &plan); err != nil {
		t.Fatal(err)
	}
	var response ownerOperationResponse
	if err := session.call(context.Background(), "operation.execute", "service-status", map[string]any{"confirmed": true}, &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.Status != "ok" || len(runner.Requests) != 1 || runner.Requests[0].Context["YARD_NAME"] != "fixture-runtime" || !strings.Contains(diagnostics.String(), selection.record.Generation) {
		t.Fatal("framed status did not use published service context")
	}
	var exact exactOperationPlan
	if err := session.call(context.Background(), "operation.plan", "service-revoke", map[string]any{"command": "test-vms", "arguments": []string{"revoke", "--slot", "1", "--expect-owner", selection.record.Owner}, "exact": true, "stepSchema": 1}, &exact); err != nil {
		t.Fatal(err)
	}
	if _, err := session.execute(context.Background(), exact); err != nil {
		t.Fatal(err)
	}
	if len(runner.Requests) != 2 || runner.Requests[1].Action != "revoke" {
		t.Fatal("framed mutation did not use existing exact operation pipeline")
	}
	if _, err := session.execute(context.Background(), exact); err == nil {
		t.Fatal("exact service operation replayed")
	}
	err = session.call(context.Background(), "operation.plan", "wrong-owner", map[string]any{"command": "test-vms", "arguments": []string{"status", "--expect-owner", "replaced-owner"}}, &denied)
	if !errors.As(err, &failure) || failure.Code != domain.PlanStaleCode {
		t.Fatalf("wrong expected owner: %v", err)
	}
}

func TestOwnerServiceControllerSelectionDoesNotReadDefaultWorkspace(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	registerStructuredOwnerFixture(t, program, "fixture-owner", "owner.example")
	if err := os.MkdirAll(filepath.Join(root, "state/yards/default"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "state/yards/default/config.env"), []byte("SSH_PORT=invalid\n"), 0o600)
	definition, _ := program.manifest.Lookup("test-vms")
	_, _, err = program.selectServiceInvocation(context.Background(), definition, []string{"status", "--owner", "fixture-owner"})
	if err != nil {
		t.Fatal(err)
	}
	loaded := *program.serviceRemote
	if loaded.Context.NestedE2EVMs || loaded.Context.AccessKind != domain.AccessRemote {
		t.Fatal("controller invented native service settings")
	}
	if _, err := program.prepareTestVMExecution(context.Background(), loaded, []string{"status", "--json"}); err != nil {
		t.Fatalf("controller applied owner admission locally: %v", err)
	}
	_, _, err = program.selectServiceInvocation(context.Background(), command.Definition{Name: "test-vms", Handler: "@test-vms"}, []string{"status", "--owner", "absent-owner"})
	if err == nil || !strings.Contains(err.Error(), "service_owner_absent") {
		t.Fatalf("absent owner fell back: %v", err)
	}
}

func TestOwnerServiceRemoteStatusUsesTransportWithoutWorkspaceAdmission(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(map[bool]string{true: "current-owner", false: "old-owner"}[supported], func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			fakeBin := filepath.Join(root, "fake-bin")
			if err := os.MkdirAll(fakeBin, 0o700); err != nil {
				t.Fatal(err)
			}
			log := writeStructuredOwnerFixture(t, fakeBin, "test-vms", nil, nil, "")
			ownerPath := filepath.Join(fakeBin, "owner.py")
			if !supported {
				body, err := os.ReadFile(ownerPath)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, ownerPath, bytes.ReplaceAll(body, []byte(",'owner-services-v1'"), nil), 0o600)
			}
			forwarded := filepath.Join(root, "forwarded")
			script := "#!/bin/sh\n" + trustedSSHMock(t) + "\ncase \"$*\" in *rpc*) exec python3 '" + ownerPath + "' \"$@\" ;; esac\nprintf '%s\\n' \"$*\" > '" + forwarded + "'\nprintf '%s\\n' '{\"schema_version\":1,\"status\":\"ok\"}'\n"
			testkit.WriteFile(t, filepath.Join(fakeBin, "ssh"), []byte(script), 0o700)
			t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
			environment = append(environment, "PATH="+os.Getenv("PATH"))
			var output, diagnostics bytes.Buffer
			incus := &testkit.Incus{}
			program, err := New(Options{RepositoryRoot: root, Arguments: []string{"-Y", "invalid-workspace", "test-vms", "status", "--json", "--owner", "fixture-owner"}, Environment: environment, WorkingDir: root, Incus: incus, Executor: incus, Stdout: &output, Stderr: &diagnostics})
			if err != nil {
				t.Fatal(err)
			}
			registerStructuredOwnerFixture(t, program, "fixture-owner", "owner.example")
			if err := os.MkdirAll(filepath.Join(root, "state/yards/default"), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "state/yards/default/config.env"), []byte("SSH_PORT=invalid\n"), 0o600)
			code := program.Run(context.Background())
			if supported {
				body, err := os.ReadFile(forwarded)
				if code != 0 || err != nil || !strings.Contains(string(body), "--expect-owner") || !strings.Contains(string(body), "fixture-owner") || !strings.Contains(output.String(), `"status":"ok"`) {
					t.Fatalf("status code=%d: %s; forward=%s %v", code, diagnostics.String(), body, err)
				}
			} else {
				if code == 0 || !strings.Contains(diagnostics.String(), "update Subyard on the owner host") {
					t.Fatalf("old owner code=%d: %s", code, diagnostics.String())
				}
				if _, err := os.Stat(forwarded); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("old owner forwarded status without capability")
				}
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(calls), "rpc.negotiate") || strings.Contains(string(calls), "operation.execute") || len(incus.ExecCalls) != 0 || len(incus.PowerUpdates) != 0 {
				t.Fatal("controller performed native work or skipped capability negotiation")
			}
		})
	}
}
