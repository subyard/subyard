package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRPCNativeLifecycleCannotReadSessionInput(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	// Incus init reads non-terminal stdin to EOF before creating an instance.
	// Exercise the real native process runner with that same behavior.
	testkit.WriteFile(t, filepath.Join(root, "scripts/02-create-project.sh"), []byte("#!/bin/sh\nexec /bin/cat\n"), 0o755)
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = writer.Close() })
	request := rpc.Request{Version: 1, Type: "request", ID: "next-query", OperationID: "next-query", Method: "system.ping"}
	if err := rpc.NewCodec(nil, writer).Write(request); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Stdin: input, Stderr: &output})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	operation := program.rpcOperation("bootstrap")
	platform := operation.initPlatform(loaded, nil).(reconcileruntime.Runtime)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := platform.ApplyStage(ctx, ports.ReconcileStageProject); err != nil {
		t.Fatalf("native process did not receive immediate EOF: %v", err)
	}
	if output.Len() != 0 || program.options.Stdin != input {
		t.Fatal("native process consumed session input or changed interactive CLI input")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	observed, err := rpc.NewCodec(input, nil).ReadRequest()
	if err != nil || observed.ID != request.ID {
		t.Fatal("native operation consumed the pending framed RPC query")
	}
}

func verandaBootstrapFixture(t *testing.T) (*rpcHandler, *initPlatformFixture, string, string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	environment = withoutCommandSetting(environment, "SSH_PORT")
	testkit.WriteFile(t, filepath.Join(root, "config/ports.env"), []byte("SSH_PORT=2222\n"), 0o600)
	preset := filepath.Join(root, "config/profiles/sample-preset/yard.env")
	if err := os.MkdirAll(filepath.Dir(preset), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, preset, []byte("SSH_PORT=2234\nENVIRONMENT_PROFILES=\n"), 0o644)
	platform := newInitPlatformFixture()
	program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: platform})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	t.Cleanup(handler.closePlans)
	return handler, platform, preset, filepath.Join(root, "state/yards/new-yard/config.env")
}

func TestVerandaBootstrapUsesNativeInitWithoutSwitchingSession(t *testing.T) {
	for _, action := range []string{"discard", "execute", "stale", "unconfirmed"} {
		t.Run(action, func(t *testing.T) {
			handler, platform, preset, target := verandaBootstrapFixture(t)
			original := handler.loaded
			environment := maps.Clone(handler.cli.env)
			value, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "bootstrap",
				Params: json.RawMessage(`{"command":"init","arguments":["--profile","sample-preset"],"exact":true,"targetYard":"new-yard"}`)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			exact := value.(exactOperationPlan)
			prepared := handler.plans["bootstrap"]
			if prepared.Loaded.Context.YardName != "new-yard" || prepared.Loaded.Context.SSHPort != 2234 ||
				exact.Plan.Confirmation != domain.ConfirmationPromptDefaultYes || len(exact.Digest) != 64 {
				t.Fatal("bootstrap did not retain native preset context and confirmation")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) || len(platform.applied) != 0 {
				t.Fatal("planning registered a yard or applied a native stage")
			}
			if !reflect.DeepEqual(handler.loaded, original) || !maps.Equal(environment, handler.cli.env) {
				t.Fatal("bootstrap switched the immutable session")
			}
			if action == "discard" {
				_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.discard", OperationID: "bootstrap", Params: json.RawMessage(`{}`)}, nil)
				if err != nil || !prepared.closed {
					t.Fatal("discard did not release bootstrap")
				}
				if _, err := os.Lstat(target); !os.IsNotExist(err) {
					t.Fatal("discard created registration")
				}
				return
			}
			confirmed := action != "unconfirmed"
			params, _ := json.Marshal(map[string]any{"confirmed": confirmed, "digest": exact.Digest})
			if action == "stale" {
				testkit.WriteFile(t, preset, []byte("SSH_PORT=2235\nENVIRONMENT_PROFILES=\n"), 0o644)
			}
			_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "bootstrap", Params: params}, nil)
			if action == "unconfirmed" {
				var fault *rpc.Error
				if !errors.As(err, &fault) || fault.Code != "confirmation_required" || handler.plans["bootstrap"] == nil {
					t.Fatal("unconfirmed bootstrap consumed the plan")
				}
				if _, err := os.Lstat(target); !os.IsNotExist(err) || len(platform.applied) != 0 {
					t.Fatal("unconfirmed bootstrap mutated owner state")
				}
				return
			}
			if action == "stale" {
				var fault *rpc.Error
				if !errors.As(err, &fault) || fault.Code != "plan_stale" || len(platform.applied) != 0 {
					t.Fatalf("stale preset reached apply: %v", err)
				}
				if _, err := os.Lstat(target); !os.IsNotExist(err) {
					t.Fatal("stale bootstrap registered a yard")
				}
			} else if err != nil {
				t.Fatal(err)
			} else if content, err := os.ReadFile(target); err != nil || string(content) != "SSH_PORT=2234\nENVIRONMENT_PROFILES=\n" || len(platform.applied) == 0 {
				t.Fatal("confirmed bootstrap did not execute native registration and stages")
			}
			_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.execute", OperationID: "bootstrap", Params: params}, nil)
			var fault *rpc.Error
			if !errors.As(err, &fault) || fault.Code != "plan_not_found" || !prepared.closed {
				t.Fatal("bootstrap replay or retained resources accepted")
			}
			if !reflect.DeepEqual(handler.loaded, original) || !maps.Equal(environment, handler.cli.env) {
				t.Fatal("bootstrap execution switched the query session")
			}
		})
	}
}

func TestVerandaBootstrapRejectsCrossYardSelectionOutsideExactProfileInit(t *testing.T) {
	handler, platform, _, target := verandaBootstrapFixture(t)
	for _, params := range []string{
		`{"command":"start","arguments":[],"exact":true,"targetYard":"new-yard"}`,
		`{"command":"init","arguments":[],"exact":true,"targetYard":"new-yard"}`,
		`{"command":"init","arguments":["--profile","sample-preset"],"targetYard":"new-yard"}`,
		`{"command":"init","arguments":["--profile","sample-preset"],"exact":true,"targetYard":"default"}`,
		`{"command":"init","arguments":["--profile","sample-preset"],"exact":true,"targetYard":"../new-yard"}`,
		`{"command":"init","arguments":["--reset","--profile","sample-preset"],"exact":true,"targetYard":"new-yard"}`,
	} {
		_, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "invalid", Params: json.RawMessage(params)}, nil)
		var fault *rpc.Error
		if !errors.As(err, &fault) || fault.Code != "invalid_params" {
			t.Fatalf("invalid target selection accepted: %v", err)
		}
	}
	handler.loaded.Context.AccessKind = domain.AccessRemote
	_, err := handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "remote", Params: json.RawMessage(`{"command":"init","arguments":["--profile","sample-preset"],"exact":true,"targetYard":"new-yard"}`)}, nil)
	var fault *rpc.Error
	if !errors.As(err, &fault) || fault.Code != "remote_owner_required" {
		t.Fatal("controller-side bootstrap accepted")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) || len(platform.applied) != 0 || len(handler.plans) != 0 {
		t.Fatal("rejected target selection mutated owner state")
	}
}

func TestVerandaProfileNamedPresetIsProtectedMetadata(t *testing.T) {
	handler, _, _ := ownerQueryFixture(t)
	preset := filepath.Join(handler.cli.options.RepositoryRoot, "config/profiles/resource-only/yard.env")
	for _, mode := range []string{"regular", "unsafe", "symlink", "missing"} {
		t.Run(mode, func(t *testing.T) {
			_ = os.Remove(preset)
			switch mode {
			case "regular":
				testkit.WriteFile(t, preset, []byte("SSH_PORT=2234\n"), 0o644)
			case "unsafe":
				testkit.WriteFile(t, preset, []byte("SSH_PORT=2234\n"), 0o666)
			case "symlink":
				if err := os.Symlink("/nonexistent", preset); err != nil {
					t.Fatal(err)
				}
			}
			result, err := handler.profileList(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, profile := range result.Profiles {
				if profile.Name == "resource-only" && profile.HasYardPreset != (mode == "regular") {
					t.Fatal("preset metadata accepted an unsafe file or missed a valid preset")
				}
			}
		})
	}
}
