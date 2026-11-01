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

func TestNativeResourceDeclarationCaptureLifetime(t *testing.T) {
	for _, reset := range []string{"inventory", "fresh-activation"} {
		t.Run(reset, func(t *testing.T) {
			c, home := nativeActivationSourceFixture(t)
			root := c.context.RepositoryRoot
			profile := filepath.Join(root, "config/profiles/fixture")
			if err := os.MkdirAll(filepath.Join(profile, "resources/fixture"), 0700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(profile, "profile.json"), `{"schema_version":1,"settings":[{"name":"FIXTURE_ADVERTISE_HOST","type":"string","scopes":["shipped","host","yard","command"],"application":"next-command","syncable":true,"optional":true},{"name":"FIXTURE_HOST_PORT","type":"port","scopes":["shipped","host","yard","command"],"application":"next-command","syncable":true,"optional":true,"minimum":1,"maximum":65535,"host_listener":true}]}`, 0600)
			descriptor := filepath.Join(profile, "resources/fixture.res")
			writeCLIFile(t, descriptor, "COMMAND=fixture\nTITLE=Fixture\nHANDLER=resources/fixture/handler.sh\nPROXY=\"fixture FIXTURE_ADVERTISE_HOST FIXTURE_HOST_PORT tcp:127.0.0.1:6768 loopback-or-tailscale\"\nENDPOINT_DEFAULTS=\"tailscale-self 6768\"\nBOOTSTRAP=profile\nACTION=\"up up bootstrap-change recreatable\"\nACTION=\"down down host-change reversible\"\nBRINGUP=up\nSHUTDOWN=down\n", 0600)
			writeCLIFile(t, filepath.Join(profile, "resources/fixture/handler.sh"), "#!/bin/sh\nexit 99\n", 0700)
			writeCLIFile(t, filepath.Join(home, "config.env"), "ENVIRONMENT_PROFILES=fixture\n", 0600)
			catalog, err := config.LoadCatalog(root)
			if err != nil {
				t.Fatal(err)
			}
			c.catalog = &catalog
			c.cli.catalog = catalog
			if _, _, err = c.CaptureInventory(context.Background()); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(c.context.DataHome, "resource-endpoints/state.json")
			if err = os.MkdirAll(filepath.Dir(state), 0700); err != nil {
				t.Fatal(err)
			}
			save := func(port string) {
				writeCLIFile(t, state, `{"schema":1,"allocations":[{"yard":"default","resource":"fixture.fixture","host":"owner.example.ts.net","port":`+port+`}]}`, 0600)
			}
			save("17678")
			loaded, err := c.load("default")
			if err != nil || loaded.Environment["FIXTURE_HOST_PORT"] != "17678" {
				t.Fatal("initial native endpoint", err)
			}
			writeCLIFile(t, descriptor, "INVALID DECLARATION\n", 0600)
			save("27678")
			loaded, err = c.load("default")
			if err != nil || loaded.Environment["FIXTURE_HOST_PORT"] != "27678" {
				t.Fatal("same observation must reuse declarations but reread saved endpoint", err)
			}
			c.activationDraft = &sourceActivationDraft{names: []string{"default"}, inputs: c.view()}
			loaded, err = c.loadActivation("default")
			if err != nil || loaded.Environment["FIXTURE_HOST_PORT"] != "27678" {
				t.Fatal("projected load must reuse declarations and current endpoint", err)
			}
			if reset == "inventory" {
				if _, _, err = c.CaptureInventory(context.Background()); err != nil {
					t.Fatal(err)
				}
				_, err = c.load("default")
			} else {
				_, err = c.CaptureActivation(context.Background(), "host-power", true)
			}
			if err == nil {
				t.Fatal("new capture reused invalidated declarations")
			}
		})
	}
}
