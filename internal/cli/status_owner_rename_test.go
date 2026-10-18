package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestStatusAdoptsTrustedOwnerRename(t *testing.T) {
	for _, cacheState := range []string{"expired", "missing"} {
		for _, view := range []string{"summary", "detail"} {
			t.Run(cacheState+"/"+view, func(t *testing.T) {
				root, environment, _ := nativeFixture(t)
				environment = append(environment, "SUBYARD_HOST_ID=local-owner")
				inventory := inventoryResult("new-owner", "default", "Remote").inventory
				_, trust := hostAddSSHFixture(t, inventory)
				writeStatusOwnerResponses(t, inventory)
				ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
				store := ownerinventory.Connections{Root: ownerRoot}
				connection := ownerinventory.Connection{
					HostID: "old-owner", Destination: "owner-alias", Trust: &trust,
					Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-remote"}},
				}
				if err := store.Write(connection); err != nil {
					t.Fatal(err)
				}
				cache := ownerinventory.Cache{Root: ownerRoot}
				if cacheState == "expired" {
					if err := cache.Write(ownerinventory.Snapshot{
						FetchedAt: time.Unix(1, 0).UTC(), Inventory: inventoryResult("old-owner", "default", "Remote").inventory,
					}); err != nil {
						t.Fatal(err)
					}
				}
				oldRouting := filepath.Join(ownerRoot, "routing", "old-owner", "default", "projects")
				if err := os.MkdirAll(oldRouting, 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(oldRouting, "project.json"), []byte("routing-state\n"), 0o600)
				arguments := []string{"status"}
				if view == "detail" {
					arguments = []string{"-Y", "new-owner/default", "status"}
				}
				for attempt := 0; attempt < 2; attempt++ {
					var stdout, stderr bytes.Buffer
					prompt := &testkit.Prompt{}
					program, err := New(Options{
						RepositoryRoot: root, Program: "yard", Arguments: arguments, Environment: environment,
						WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt, Incus: &testkit.Incus{},
					})
					if err != nil {
						t.Fatal(err)
					}
					if code := program.Run(context.Background()); code != 0 {
						t.Fatalf("status attempt %d: code=%d stderr=%q stdout=%q", attempt, code, stderr.String(), stdout.String())
					}
					if !strings.Contains(stdout.String(), "new-owner/default") || strings.Contains(stdout.String(), "old-owner/") || stderr.Len() != 0 || len(prompt.Requests) != 0 {
						t.Fatalf("unexpected status: stdout=%q stderr=%q prompts=%#v", stdout.String(), stderr.String(), prompt.Requests)
					}
					if view == "summary" && !strings.Contains(stdout.String(), "local-owner/default") {
						t.Fatalf("local summary disappeared: %s", stdout.String())
					}
					connections, err := store.ListReadOnly()
					if err != nil || len(connections) != 1 || connections[0].HostID != "new-owner" || connections[0].Trust == nil || connections[0].Trust.Fingerprint != trust.Fingerprint || connections[0].Yards["default"].SSHHost != "yard-remote" {
						t.Fatalf("adopted connection=%#v err=%v", connections, err)
					}
					snapshot, err := cache.Read("new-owner")
					if err != nil || snapshot.Inventory.HostID != "new-owner" || len(snapshot.Inventory.Yards[0].Projects) != 1 {
						t.Fatalf("adopted cache=%#v err=%v", snapshot, err)
					}
					for _, path := range []string{
						filepath.Join(ownerRoot, "connections", "old-owner.json"), filepath.Join(ownerRoot, "owners", "old-owner.json"), filepath.Join(ownerRoot, "routing", "old-owner"),
					} {
						if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("old identity remains at %s: %v", path, err)
						}
					}
					if payload, err := os.ReadFile(filepath.Join(ownerRoot, "routing", "new-owner", "default", "projects", "project.json")); err != nil || string(payload) != "routing-state\n" {
						t.Fatalf("routing=%q err=%v", payload, err)
					}
				}
			})
		}
	}
}

func TestStatusRecoversInterruptedOwnerRename(t *testing.T) {
	for _, view := range []string{"summary", "detail"} {
		t.Run(view, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			inventory := inventoryResult("new-owner", "default", "Remote").inventory
			_, trust := hostAddSSHFixture(t, inventory)
			writeStatusOwnerResponses(t, inventory)
			ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
			store := ownerinventory.Connections{Root: ownerRoot}
			oldConnection := ownerinventory.Connection{HostID: "old-owner", Destination: "owner-alias", Trust: &trust}
			if err := store.Register(oldConnection, ownerinventory.Snapshot{
				FetchedAt: time.Unix(1, 0).UTC(), Inventory: inventoryResult("old-owner", "default", "Remote").inventory,
			}); err != nil {
				t.Fatal(err)
			}
			oldRouting := filepath.Join(ownerRoot, "routing", "old-owner", "default", "projects", "project.json")
			if err := os.MkdirAll(filepath.Dir(oldRouting), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, oldRouting, []byte("routing-state\n"), 0o600)
			newConnection := oldConnection
			newConnection.HostID = "new-owner"
			// Simulate interruption after the durable journal and new connection
			// have been published, before cache/routing and old identity cleanup.
			journal, err := json.Marshal(map[string]any{
				"schemaVersion": 1, "oldConnection": oldConnection, "newConnection": newConnection,
				"snapshot": ownerinventory.Snapshot{FetchedAt: time.Now().UTC(), Inventory: inventory}, "routingWasPresent": true,
			})
			if err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(ownerRoot, "host-id-adoption.json")
			testkit.WriteFile(t, journalPath, journal, 0o600)
			connectionJSON, err := json.Marshal(newConnection)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(ownerRoot, "connections", "new-owner.json"), connectionJSON, 0o600)
			if _, err := store.ListReadOnly(); err == nil {
				t.Fatal("interrupted fixture did not expose duplicate owner destinations")
			}
			arguments := []string{"status"}
			if view == "detail" {
				arguments = []string{"-Y", "new-owner/default", "status"}
			}
			var previous map[string][]byte
			for attempt := 0; attempt < 2; attempt++ {
				var stdout, stderr bytes.Buffer
				prompt := &testkit.Prompt{}
				program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: arguments, Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt, Incus: &testkit.Incus{}})
				if err != nil {
					t.Fatal(err)
				}
				if attempt == 0 {
					loaded, err := program.loadContext("default")
					if err != nil {
						t.Fatal(err)
					}
					results := program.allOwnerInventoriesReadOnly(context.Background(), loaded, false)
					if len(results) != 2 || results[1].err == nil {
						t.Fatalf("strict read accepted interrupted migration: %#v", results)
					}
					if payload, err := os.ReadFile(journalPath); err != nil || !bytes.Equal(payload, journal) {
						t.Fatalf("strict read changed journal: %v", err)
					}
				}
				if code := program.Run(context.Background()); code != 0 || !strings.Contains(stdout.String(), "new-owner/default") || strings.Contains(stdout.String(), "old-owner/") || stderr.Len() != 0 || len(prompt.Requests) != 0 {
					t.Fatalf("recovery attempt %d code=%d stdout=%q stderr=%q prompts=%#v", attempt, code, stdout.String(), stderr.String(), prompt.Requests)
				}
				for _, path := range []string{journalPath, filepath.Join(ownerRoot, "connections", "old-owner.json"), filepath.Join(ownerRoot, "owners", "old-owner.json"), filepath.Join(ownerRoot, "routing", "old-owner")} {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("recovery left old/pending state at %s: %v", path, err)
					}
				}
				connections, err := store.ListReadOnly()
				if err != nil || len(connections) != 1 || connections[0].HostID != "new-owner" || connections[0].Trust == nil || connections[0].Trust.Fingerprint != trust.Fingerprint {
					t.Fatalf("recovered connections=%#v err=%v", connections, err)
				}
				snapshot, err := (ownerinventory.Cache{Root: ownerRoot}).Read("new-owner")
				if err != nil || snapshot.Inventory.HostID != "new-owner" {
					t.Fatalf("recovered cache=%#v err=%v", snapshot, err)
				}
				newRouting := filepath.Join(ownerRoot, "routing", "new-owner", "default", "projects", "project.json")
				if payload, err := os.ReadFile(newRouting); err != nil || string(payload) != "routing-state\n" {
					t.Fatalf("recovered routing=%q err=%v", payload, err)
				}
				current := make(map[string][]byte)
				for _, path := range []string{filepath.Join(ownerRoot, "connections", "new-owner.json"), filepath.Join(ownerRoot, "owners", "new-owner.json"), newRouting} {
					payload, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if previous != nil && !bytes.Equal(previous[path], payload) {
						t.Fatalf("repeated status changed recovered state at %s", path)
					}
					current[path] = payload
				}
				previous = current
			}
		})
	}
}

func TestStatusOwnerRenameRefusalsPreserveState(t *testing.T) {
	for _, scenario := range []string{"changed-key", "collision", "untrusted", "offline", "fresh", "strict-read"} {
		t.Run(scenario, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			inventory := inventoryResult("new-owner", "default", "Remote").inventory
			bin, trust := hostAddSSHFixture(t, inventory)
			environment = append(environment, "SSH_ASSESS_LINE="+trust.KnownHostsLine, "SSH_RPC_RESPONSE="+os.Getenv("SSH_RPC_RESPONSE"))
			ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
			store := ownerinventory.Connections{Root: ownerRoot}
			connection := ownerinventory.Connection{HostID: "old-owner", Destination: "owner-alias", Trust: &trust}
			if scenario == "untrusted" {
				connection.Trust = nil
				testkit.WriteFile(t, filepath.Join(bin, "known_hosts"), []byte(trust.KnownHostsLine+"\n"), 0o600)
			}
			if err := store.Write(connection); err != nil {
				t.Fatal(err)
			}
			fetchedAt := time.Unix(1, 0).UTC()
			if scenario == "fresh" {
				fetchedAt = time.Now().UTC()
			}
			cache := ownerinventory.Cache{Root: ownerRoot}
			if err := cache.Write(ownerinventory.Snapshot{FetchedAt: fetchedAt, Inventory: inventoryResult("old-owner", "default", "Remote").inventory}); err != nil {
				t.Fatal(err)
			}
			if scenario == "collision" {
				if err := store.Register(ownerinventory.Connection{HostID: "new-owner", Destination: "other-owner", Trust: &trust}, ownerinventory.Snapshot{FetchedAt: time.Now(), Inventory: inventory}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "changed-key" || scenario == "offline" || scenario == "fresh" {
				message := "offline"
				if scenario == "changed-key" {
					message = "REMOTE HOST IDENTIFICATION HAS CHANGED"
				}
				testkit.WriteFile(t, filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nif [ \"${1-}\" = -G ]; then\n  printf 'hostname owner-alias\\nport 22\\nuserknownhostsfile /dev/null\\n'\n  exit 0\nfi\nprintf '%s\\n' '"+message+"' >&2\nexit 255\n"), 0o700)
			}
			routingPath := filepath.Join(ownerRoot, "routing", "old-owner", "default", "projects", "project.json")
			if err := os.MkdirAll(filepath.Dir(routingPath), 0o700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, routingPath, []byte("routing-state\n"), 0o600)
			paths := []string{filepath.Join(ownerRoot, "connections", "old-owner.json"), filepath.Join(ownerRoot, "owners", "old-owner.json"), routingPath}
			if scenario == "collision" {
				paths = append(paths, filepath.Join(ownerRoot, "connections", "new-owner.json"), filepath.Join(ownerRoot, "owners", "new-owner.json"))
			}
			before := make(map[string][]byte)
			for _, path := range paths {
				payload, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = payload
			}
			var stdout, stderr bytes.Buffer
			prompt := &testkit.Prompt{}
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"status"}, Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt, Incus: &testkit.Incus{}})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "strict-read" {
				loaded, err := program.loadContext("default")
				if err != nil {
					t.Fatal(err)
				}
				results := program.allOwnerInventoriesReadOnly(context.Background(), loaded, true)
				if len(results) != 2 || !results[1].stale || !errors.Is(results[1].err, ownerinventory.ErrIntegrity) || results[1].inventory.HostID != "old-owner" {
					t.Fatalf("strict read=%#v", results)
				}
			} else {
				wantCode := 1
				if scenario == "fresh" {
					wantCode = 0
				}
				if code := program.Run(context.Background()); code != wantCode || !strings.Contains(stdout.String(), "old-owner/default") || len(prompt.Requests) != 0 {
					t.Fatalf("status code=%d stdout=%q stderr=%q prompts=%#v", code, stdout.String(), stderr.String(), prompt.Requests)
				}
				if scenario == "untrusted" && (!strings.Contains(stderr.String(), "no managed SSH trust") || !strings.Contains(stderr.String(), "yard host repair old-owner")) {
					t.Fatalf("legacy mismatch omitted confirmed repair guidance: %s", stderr.String())
				}
				if scenario == "changed-key" && (!strings.Contains(stderr.String(), "integrity violation") || !strings.Contains(stderr.String(), "yard host repair")) {
					t.Fatalf("changed key omitted integrity diagnostic: %s", stderr.String())
				}
				if scenario == "collision" && !strings.Contains(stderr.String(), "already registered") {
					t.Fatalf("collision diagnostic: %s", stderr.String())
				}
				if scenario == "fresh" && stderr.Len() != 0 {
					t.Fatalf("fresh cache attempted SSH: %s", stderr.String())
				}
				if scenario == "changed-key" || scenario == "untrusted" {
					stdout.Reset()
					stderr.Reset()
					program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"-Y", "old-owner/default", "status"}, Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt, Incus: &testkit.Incus{}})
					if err != nil {
						t.Fatal(err)
					}
					if code := program.Run(context.Background()); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "integrity violation") || !strings.Contains(stderr.String(), "yard host repair") {
						t.Fatalf("detail ignored integrity failure: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
					}
				}
			}
			for path, payload := range before {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(payload, after) {
					t.Fatalf("state changed at %s: %v", path, err)
				}
			}
			if scenario != "collision" {
				if _, err := os.Lstat(filepath.Join(ownerRoot, "connections", "new-owner.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("new identity was adopted: %v", err)
				}
			}
		})
	}
}

func TestHostRepairConfirmsLegacyTrustAndOwnerRename(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, filepath.Join(configHome, "host-id"), "local-owner\n", 0o600)
	inventory := inventoryResult("new-owner", "default", "Remote").inventory
	_, trust := hostAddSSHFixture(t, inventory)
	ownerRoot := filepath.Join(home, ".subyard", "owner-inventory")
	store := ownerinventory.Connections{Root: ownerRoot}
	if err := store.RegisterLegacy(ownerinventory.Connection{
		HostID: "old-owner", Destination: "owner-alias", Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-remote"}},
	}, ownerinventory.Snapshot{FetchedAt: time.Unix(1, 0).UTC(), Inventory: inventoryResult("old-owner", "default", "Remote").inventory}); err != nil {
		t.Fatal(err)
	}
	oldRouting := filepath.Join(ownerRoot, "routing", "old-owner", "default", "projects", "project.json")
	if err := os.MkdirAll(filepath.Dir(oldRouting), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, oldRouting, []byte("routing-state\n"), 0o600)
	for _, consent := range []bool{false, true} {
		prompt := &testkit.Prompt{Answers: []bool{consent}}
		var stdout, stderr bytes.Buffer
		program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "repair", "old-owner"}, Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Prompt: prompt})
		if err != nil {
			t.Fatal(err)
		}
		wantCode := 1
		if consent {
			wantCode = 0
		}
		if code := program.Run(context.Background()); code != wantCode || len(prompt.Requests) != 1 {
			t.Fatalf("repair consent=%t code=%d stderr=%q prompts=%#v", consent, code, stderr.String(), prompt.Requests)
		}
		consequences := strings.Join(prompt.Requests[0].Consequences, "\n")
		for _, required := range []string{"unmanaged", "old-owner", "new-owner", trust.Fingerprint} {
			if !strings.Contains(consequences, required) {
				t.Fatalf("repair omitted %q: %s", required, consequences)
			}
		}
		connections, err := store.ListReadOnly()
		if err != nil || len(connections) != 1 {
			t.Fatalf("repair connections=%#v err=%v", connections, err)
		}
		if !consent {
			if connections[0].HostID != "old-owner" || connections[0].Trust != nil {
				t.Fatalf("cancelled repair changed identity/trust: %#v", connections)
			}
			continue
		}
		if connections[0].HostID != "new-owner" || connections[0].Trust == nil || connections[0].Trust.Fingerprint != trust.Fingerprint || connections[0].Yards["default"].SSHHost != "yard-remote" {
			t.Fatalf("confirmed repair connections=%#v", connections)
		}
		if snapshot, err := (ownerinventory.Cache{Root: ownerRoot}).Read("new-owner"); err != nil || snapshot.Inventory.HostID != "new-owner" {
			t.Fatalf("repaired cache=%#v err=%v", snapshot, err)
		}
		if payload, err := os.ReadFile(filepath.Join(ownerRoot, "routing", "new-owner", "default", "projects", "project.json")); err != nil || string(payload) != "routing-state\n" {
			t.Fatalf("repaired routing=%q err=%v", payload, err)
		}
		for _, path := range []string{filepath.Join(ownerRoot, "connections", "old-owner.json"), filepath.Join(ownerRoot, "owners", "old-owner.json"), filepath.Join(ownerRoot, "routing", "old-owner")} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old identity remains at %s: %v", path, err)
			}
		}
	}
}

func writeStatusOwnerResponses(t *testing.T, inventory domain.OwnerInventory, readiness ...*domain.ReleaseReadiness) {
	t.Helper()
	var framed bytes.Buffer
	codec := rpc.NewCodec(bytes.NewReader(nil), &framed)
	capabilities := []string{ownerinventory.Capability, "yard-status"}
	if len(readiness) != 0 {
		capabilities = append(capabilities, ownerinventory.ReleaseReadinessCapability)
	}
	responses := []rpc.Response{
		{Version: rpc.ProtocolVersion, Type: "response", ID: "negotiate", Result: map[string]any{"capabilities": capabilities}},
		{Version: rpc.ProtocolVersion, Type: "response", ID: "inventory", Result: inventory},
		{Version: rpc.ProtocolVersion, Type: "response", ID: "status", Result: domain.YardStatus{State: "RUNNING", ProjectCount: 1, Context: domain.Context{YardName: "default", DevUser: "dev", SSHPort: 2222}}},
	}
	if len(readiness) != 0 {
		responses = append(responses, rpc.Response{Version: rpc.ProtocolVersion, Type: "response", ID: "release-readiness", Result: readiness[0]})
	}
	for _, response := range responses {
		if err := codec.Write(response); err != nil {
			t.Fatal(err)
		}
	}
	testkit.WriteFile(t, os.Getenv("SSH_RPC_RESPONSE"), framed.Bytes(), 0o600)
}

func TestActiveOwnerQueryAdoptsRenameAndPreservesEndpointAndRoute(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	inventory := inventoryResult("new-owner", "default", "Remote").inventory
	_, trust := hostAddSSHFixture(t, inventory)
	writeStatusOwnerResponses(t, inventory)
	ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
	store := ownerinventory.Connections{Root: ownerRoot}
	connection := ownerinventory.Connection{HostID: "old-owner", Destination: "owner-alias", Trust: &trust, Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-remote"}}}
	if err := store.Register(connection, ownerinventory.Snapshot{FetchedAt: time.Unix(1, 0).UTC(), Inventory: inventoryResult("old-owner", "default", "Remote").inventory}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"yards"}, Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Incus: &testkit.Incus{}})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 || !strings.Contains(stdout.String(), "new-owner/default") {
		t.Fatalf("query code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	connections, err := store.ListReadOnly()
	if err != nil || len(connections) != 1 || connections[0].Destination != connection.Destination || connections[0].HostID != "new-owner" || connections[0].Yards["default"].SSHHost != "yard-remote" {
		t.Fatalf("connection=%+v err=%v", connections, err)
	}
}
