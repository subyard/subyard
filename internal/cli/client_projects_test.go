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

	"github.com/Subyard/Subyard/internal/clientprojects"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

func clientProjectsFixture(t *testing.T, options ...fixtureOption) *cliFixture {
	t.Helper()
	// These constructors leave WorkingDir empty, as before the fixture.
	fixture := newFixture(t, append([]fixtureOption{withWorkingDir(""), withYards("named", "remote"), withEnv("SUBYARD_HOST_ID=local-owner")}, options...)...)
	fixture.Write("config/commands.registry", fixture.Read("config/commands.registry")+"desktop||@client-projects|synthetic|local|mutate|never|public|projects|integration|desktop <command>|export projects|--check --config --help|export open\n")
	return fixture
}

func TestClientProjectsSelectsExactlyOneLocalYard(t *testing.T) {
	for _, selector := range []string{"default", "named", "local-owner/default", "local-owner/named"} {
		t.Run(selector, func(t *testing.T) {
			fixture := clientProjectsFixture(t, withEnv("SUBYARD_NO_AUDIT="))
			fixture.Write(filepath.Join(fixture.ConfigHome, "yards", "named", "config.env"), "SSH_PORT=2223\n")
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
				"subyard/yard": {Status: "RUNNING"}, "subyard-named/yard-named": {Status: "RUNNING"},
			}}
			program := fixture.New(Options{Arguments: []string{"-Y", selector, "desktop", "export", "--check"}, Incus: incus})
			base, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"default", "named"} {
				loaded, err := program.loadInventoryLoaded(name, base)
				if err != nil {
					t.Fatal(err)
				}
				store, err := state.NewFileStore(loaded.Context.Paths.StateDir)
				if err != nil {
					t.Fatal(err)
				}
				record := projectRemovalRecord(domain.ProjectSync)
				record.Target = "synthetic-profile"
				if err := store.Put(context.Background(), record); err != nil {
					t.Fatal(err)
				}
			}
			want := "default"
			if strings.HasSuffix(selector, "named") {
				want = "named"
			}
			var received clientprojects.Export
			applies, opens, probes := 0, 0, 0
			program.options.ClientYardProbe = func(_ context.Context, route domain.Context, yard domain.OwnerYard) error {
				probes++
				if route.YardName != want || yard.Name != want {
					t.Fatalf("wrong route: %#v %#v", route, yard)
				}
				return nil
			}
			program.options.ClientProjects = func(_ context.Context, tool string, export clientprojects.Export, override string) (clientprojects.Plan, error) {
				if tool != "synthetic" || override != "" {
					t.Fatal("client dispatch lost arguments")
				}
				received = export
				return clientprojects.Plan{Target: "/synthetic/config", Changed: true, Added: 1,
					Apply: func(context.Context) error { applies++; return nil }, Open: func(context.Context) error { opens++; return nil }}, nil
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("export preview exit=%d", code)
			}
			if received.HostID != "local-owner" || received.Yard != want || len(received.Projects) != 1 ||
				received.Projects[0].Path != state.YardPath(projectRemovalRecord(domain.ProjectSync).ProjectID) || probes != 1 || applies != 0 || opens != 0 {
				t.Fatalf("scope/preview: %#v probes=%d apply=%d open=%d", received, probes, applies, opens)
			}
			if _, err := os.Stat(filepath.Join(fixture.DataHome, "logs")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preview created audit files or locks: %v", err)
			}
		})
	}
}

func TestClientProjectsOpenAndRetryAfterUnchangedExport(t *testing.T) {
	fixture := clientProjectsFixture(t)
	var stdout, stderr bytes.Buffer
	changes := []bool{true, false, false}
	opens, applies := 0, 0
	for index, changed := range changes {
		incus := lifecycleIncus()
		instance := incus.Instances["subyard/yard"]
		instance.Status = "RUNNING"
		incus.Instances["subyard/yard"] = instance
		program := fixture.New(Options{
			Arguments: []string{"desktop", "open"}, Incus: incus, Stdout: &stdout, Stderr: &stderr,
			ClientYardProbe: func(context.Context, domain.Context, domain.OwnerYard) error { return nil },
			ClientProjects: func(context.Context, string, clientprojects.Export, string) (clientprojects.Plan, error) {
				return clientprojects.Plan{Target: "/synthetic/config", Changed: changed,
					Apply: func(context.Context) error { applies++; return nil },
					Open: func(context.Context) error {
						opens++
						if index == 0 {
							return errors.New("synthetic launch failed")
						}
						return nil
					}}, nil
			},
		})
		code := program.Run(context.Background())
		if (index == 0 && code != 1) || (index != 0 && code != 0) {
			t.Fatalf("retry %d: exit=%d stderr=%s", index, code, stderr.String())
		}
	}
	if opens != 3 || applies != 3 || strings.Count(stdout.String(), "Desktop import requested") != 2 {
		t.Fatalf("explicit open/retry applies=%d opens=%d output=%s", applies, opens, stdout.String())
	}
}

func TestClientProjectsRemoteFreshAndFailClosed(t *testing.T) {
	for _, scenario := range []string{"canonical", "legacy", "unreachable", "identity", "missing-route", "probe", "ambiguous", "drift"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := clientProjectsFixture(t)
			live := inventoryResult("remote-owner", "default", "Live").inventory
			live.Yards[0].Projects[0].Target = "synthetic-profile"
			if scenario == "identity" {
				live.HostID = "renamed-owner"
			}
			bin, trust := hostAddSSHFixture(t, live)
			if scenario == "unreachable" {
				testkit.WriteFile(t, filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 255\n"), 0o700)
			}
			configHome := fixture.ConfigHome
			store := ownerinventory.Connections{Root: filepath.Join(fixture.DataHome, "owner-inventory")}
			connection := ownerinventory.Connection{HostID: "remote-owner", Destination: "owner-alias", Trust: &trust,
				Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-remote"}}}
			if scenario == "missing-route" {
				connection.Yards = nil
			}
			if scenario == "ambiguous" {
				live.Yards[0].Name = "named"
				writeHostRPCFixture(t, filepath.Join(bin, "response"), live)
				fixture.Write(filepath.Join(configHome, "yards", "named", "config.env"), "SSH_PORT=2223\n")
			}
			if err := store.Write(connection); err != nil {
				t.Fatal(err)
			}
			// A cached older project must never substitute for current RPC results.
			cache := ownerinventory.Cache{Root: store.Root}
			if err := cache.Write(ownerinventory.Snapshot{FetchedAt: time.Now(), Inventory: inventoryResult("remote-owner", "default", "Old").inventory}); err != nil {
				t.Fatal(err)
			}
			cached, _ := os.ReadFile(filepath.Join(store.Root, "owners", "remote-owner.json"))
			// The canonical target must not fetch this unrelated unreachable owner.
			if err := store.Write(ownerinventory.Connection{HostID: "offline", Destination: "offline-alias", Trust: &trust}); err != nil {
				t.Fatal(err)
			}
			original, _ := os.ReadFile(filepath.Join(bin, "ssh"))
			testkit.WriteFile(t, filepath.Join(bin, "ssh"), []byte(strings.Replace(string(original), "#!/bin/sh\n", "#!/bin/sh\nfor arg do [ \"$arg\" != offline-alias ] || exit 73; done\n", 1)), 0o700)
			testkit.WriteFile(t, filepath.Join(store.Root, "registration.json"), []byte("pending journal must not recover\n"), 0o600)
			selector := "remote-owner/default"
			if scenario == "legacy" {
				selector = "remote"
				fixture.Write(filepath.Join(configHome, "yards", "remote", "config.env"), "ACCESS_KIND=remote\nOWNER_ENDPOINT=owner-alias\nOWNER_YARD_NAME=default\nSSH_HOST=yard-remote\n")
			}
			if scenario == "ambiguous" {
				selector = "named"
			}
			applies, prepared := 0, 0
			var stdout, stderr bytes.Buffer
			incus := lifecycleIncus()
			instance := incus.Instances["subyard/yard"]
			instance.Status = "RUNNING"
			incus.Instances["subyard/yard"] = instance
			incus.Instances["subyard-named/yard-named"] = ports.InstanceInfo{Status: "RUNNING"}
			program := fixture.New(Options{
				Arguments: []string{"-Y", selector, "desktop", "export"}, Incus: incus, Stdout: &stdout, Stderr: &stderr,
				ClientYardProbe: func(_ context.Context, route domain.Context, yard domain.OwnerYard) error {
					if scenario == "probe" {
						return errors.New("L1 unavailable")
					}
					if route.SSHHost != "yard-remote" {
						t.Fatalf("route=%#v", route)
					}
					return nil
				}, ClientProjects: func(_ context.Context, _ string, export clientprojects.Export, _ string) (clientprojects.Plan, error) {
					prepared++
					if len(export.Projects) != 1 || export.Projects[0].Name != "Live" {
						t.Fatalf("stale inventory: %#v", export)
					}
					if scenario == "drift" {
						writeHostRPCFixture(t, filepath.Join(bin, "response"), inventoryResult("remote-owner", "default", "Changed").inventory)
					}
					return clientprojects.Plan{Target: "/synthetic/config", Changed: true, Apply: func(context.Context) error { applies++; return nil }}, nil
				},
			})
			code := program.Run(context.Background())
			wantOK := scenario == "canonical" || scenario == "legacy"
			wantPrepared := 0
			if wantOK || scenario == "drift" {
				wantPrepared = 1
			}
			if prepared != wantPrepared || (wantOK && (code != 0 || applies != 1)) || (!wantOK && (code == 0 || applies != 0)) {
				t.Fatalf("exit=%d prepare=%d apply=%d stderr=%s", code, prepared, applies, stderr.String())
			}
			if scenario == "drift" && !strings.Contains(stderr.String(), "selected yard inventory or SSH route changed") {
				t.Fatalf("missing stale inventory diagnostic: %s", stderr.String())
			}
			after, _ := os.ReadFile(filepath.Join(store.Root, "owners", "remote-owner.json"))
			if !bytes.Equal(cached, after) {
				t.Fatal("export rewrote owner cache")
			}
			journal, _ := os.ReadFile(filepath.Join(store.Root, "registration.json"))
			if string(journal) != "pending journal must not recover\n" {
				t.Fatal("export recovered owner registration")
			}
			if _, err := os.Stat(filepath.Join(store.Root, "tmp")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("read-only inventory transport created persistent temp state")
			}
		})
	}
}

func TestClientYardProbeRejectsOwnerOrMissingAlias(t *testing.T) {
	for _, scenario := range []string{"yard", "owner", "missing", "unreachable"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			body := "hostname yard-synthetic\nuser dev\nport 2222\n"
			if scenario == "missing" {
				body = "hostname yard-synthetic\nuser dev\nport 22\n"
			}
			probe := testkit.Reply{Stdout: "yard-synthetic\ndev\nsubyard-workspaces\n"}
			if scenario == "owner" {
				probe = testkit.Reply{Stdout: "owner-host\ndev\nsubyard-workspaces\n"}
			}
			if scenario == "unreachable" {
				probe = testkit.Reply{Exit: 255}
			}
			testkit.NewExecFake(t, root, "ssh").
				On(testkit.Match{Prefix: []string{"-G"}}, testkit.Reply{Stdout: body}).
				On(testkit.Match{}, probe)
			t.Setenv("PATH", root+":"+os.Getenv("PATH"))
			program := &CLI{env: map[string]string{}}
			err := program.probeClientYard(context.Background(), domain.Context{SSHHost: "synthetic-alias"}, domain.OwnerYard{Instance: "yard-synthetic", DevUser: "dev", SSHPort: 2222})
			if (scenario == "yard" && err != nil) || (scenario != "yard" && err == nil) {
				t.Fatalf("probe=%v", err)
			}
		})
	}
}

func TestClientProjectsRPCRefusedBeforePreparation(t *testing.T) {
	fixture := clientProjectsFixture(t)
	program := fixture.New(Options{
		ClientYardProbe: func(context.Context, domain.Context, domain.OwnerYard) error {
			t.Fatal("RPC reached the native SSH probe")
			return nil
		}, ClientProjects: func(context.Context, string, clientprojects.Export, string) (clientprojects.Plan, error) {
			t.Fatal("RPC reached the native client adapter")
			return clientprojects.Plan{}, nil
		},
	})
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded}
	defer handler.closePlans()
	for _, arguments := range [][]string{{"export", "--check"}, {"export"}, {"open"}} {
		for _, exact := range []bool{false, true} {
			params, err := json.Marshal(map[string]any{"command": "desktop", "arguments": arguments, "exact": exact})
			if err != nil {
				t.Fatal(err)
			}
			_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "desktop-refused", Params: params}, nil)
			fault, ok := err.(*rpc.Error)
			if !ok || fault.Code != "interactive_or_payload_command" {
				t.Fatalf("RPC arguments=%v exact=%v: %v", arguments, exact, err)
			}
		}
	}
}
