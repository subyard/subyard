package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/rpc"
)

func TestOperationConfigResourceCaptureLifetime(t *testing.T) {
	root, environment, _ := resourceCommandFixture(t)
	writeCLIFile(t, filepath.Join(root, "config", "profiles", "fixture", "profile.json"), `{"schema_version":1}`, 0o600)
	environment = append(environment, "ENVIRONMENT_PROFILES=fixture")
	program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	options := config.LoadOptions{RepositoryRoot: root, OperatorHome: environmentValue(environment, "HOME"), Environment: program.baseEnv}
	operation := program.rpcOperation("first")
	options.Resources = operation.operationResourceRegistry
	loaded, err := config.Load(options)
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	call := rpc.Call{Method: "owner.inventory", OperationID: "inventory-first", Params: json.RawMessage(`{}`)}
	if _, err := handler.Handle(context.Background(), call, nil); err != nil {
		t.Fatal(err)
	}
	if program.configResources != nil {
		t.Fatal("inventory query stored operation declarations on session")
	}

	descriptor := filepath.Join(root, "config", "profiles", "fixture", "resources", "demo.res")
	writeCLIFile(t, descriptor, "INVALID DECLARATION\n", 0o600)
	call.OperationID = "inventory-second"
	if _, err := handler.Handle(context.Background(), call, nil); err == nil {
		t.Fatal("next inventory query reused old declarations")
	}

	// Multiple yard loads use the capture, without rereading package declarations.
	if err := os.MkdirAll(filepath.Join(loaded.Context.Paths.ConfigHome, "yards", "another"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(loaded.Context.Paths.ConfigHome, "yards", "another", "config.env"), "SSH_PORT=2223\n", 0o600)
	options.YardName = "another"
	if _, err := config.Load(options); err != nil {
		t.Fatalf("same operation reread declarations: %v", err)
	}
	options.Resources = program.rpcOperation("second").operationResourceRegistry
	if _, err := config.Load(options); err == nil {
		t.Fatal("new RPC operation reused old candidate declarations")
	}
	otherRoot, otherEnvironment, _ := nativeFixture(t)
	if err := os.MkdirAll(filepath.Join(otherRoot, "config", "profiles", "fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(otherRoot, "config", "profiles", "fixture", "profile.json"), `{"schema_version":1}`, 0o600)
	otherEnvironment = append(otherEnvironment, "ENVIRONMENT_PROFILES=fixture")
	options.Resources = operation.operationResourceRegistry
	options.RepositoryRoot = otherRoot
	options.YardName = "default"
	options.OperatorHome = environmentValue(otherEnvironment, "HOME")
	options.Environment = environmentMap(otherEnvironment)
	if _, err := config.Load(options); err != nil {
		t.Fatalf("new root: %v", err)
	}
	if len(operation.configResources.Definitions()) != 0 {
		t.Fatal("registry leaked across roots")
	}
}
