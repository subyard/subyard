package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/state"
	"github.com/Subyard/Subyard/internal/testkit"
)

func inventoryResult(hostID, yard, project string) ownerInventoryResult {
	projects := []domain.OwnerProject{}
	if project != "" {
		projects = append(projects, domain.OwnerProject{
			ProjectID: strings.ToLower(project) + "-id", Name: project, Mode: "sync", Target: "yard",
		})
	}
	return ownerInventoryResult{inventory: domain.OwnerInventory{
		Schema: domain.OwnerInventorySchema, HostID: hostID, ObservedAt: time.Now(),
		Yards: []domain.OwnerYard{{
			Name: yard, Kind: "container", Instance: "subyard-" + yard, State: "RUNNING",
			SSHPort: 2222, DevUser: "dev", Projects: projects,
		}},
	}}
}

func TestCompactProjectListOwner(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty"},
		{name: "nineteen", value: strings.Repeat("a", 19), want: strings.Repeat("a", 19)},
		{name: "twenty", value: strings.Repeat("b", 20), want: strings.Repeat("b", 20)},
		{name: "twenty-one", value: strings.Repeat("c", 21), want: strings.Repeat("c", 17) + "..."},
		{
			name:  "uuid",
			value: "5034c950-74d0-46c4-9428-b7835e602109",
			want:  "5034c950-74d0-46c...",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := compactProjectListOwner(test.value); got != test.want {
				t.Fatalf("compactProjectListOwner(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestCanonicalYardIdentity(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(configHome, "host-id"), "local-owner\n", 0o600)
	if err := (ownerinventory.Connections{Root: filepath.Join(dataHome, "owner-inventory")}).Write(
		ownerinventory.Connection{
			HostID: "remote-owner", Destination: "dev@remote.example",
			Yards: map[string]ownerinventory.YardRoute{
				"default":      {SSHHost: "yard-remote"},
				"sample-build": {SSHHost: "yard-remote-sample-build"},
			},
		},
	); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		yard domain.Context
		want string
	}{
		{
			name: "local default",
			yard: domain.Context{YardName: "default", AccessKind: domain.AccessLocal},
			want: "local-owner/default",
		},
		{
			name: "local named",
			yard: domain.Context{YardName: "sample-build", AccessKind: domain.AccessLocal},
			want: "local-owner/sample-build",
		},
		{
			name: "remote default explicit",
			yard: domain.Context{
				YardName: "default", AccessKind: domain.AccessRemote,
				OwnerEndpoint: "dev@remote.example", OwnerYardName: "default",
			},
			want: "remote-owner/default",
		},
		{
			name: "remote default implicit",
			yard: domain.Context{
				YardName: "local-route-name", AccessKind: domain.AccessRemote,
				OwnerEndpoint: "dev@remote.example",
			},
			want: "remote-owner/default",
		},
		{
			name: "remote named",
			yard: domain.Context{
				YardName: "sample-build", AccessKind: domain.AccessRemote,
				OwnerEndpoint: "dev@remote.example", OwnerYardName: "sample-build",
			},
			want: "remote-owner/sample-build",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.yard.Paths.ConfigHome = configHome
			test.yard.Paths.DataHome = dataHome
			got, err := canonicalYardIdentity(config.Loaded{Context: test.yard})
			if err != nil || got != test.want {
				t.Fatalf("canonicalYardIdentity() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestCanonicalYardIdentityRejectsUnknownRemoteOwner(t *testing.T) {
	root := t.TempDir()
	_, err := canonicalYardIdentity(config.Loaded{Context: domain.Context{
		YardName: "default", AccessKind: domain.AccessRemote, OwnerEndpoint: "dev@missing.example",
		Paths: domain.RuntimePaths{
			ConfigHome: filepath.Join(root, "config"),
			DataHome:   filepath.Join(root, "data"),
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "canonical owner") {
		t.Fatalf("unknown remote owner was accepted: %v", err)
	}
}

func TestReadOnlyOwnerInventoriesDoNotRefreshOrMigrateConnections(t *testing.T) {
	fixture := newFixture(t)
	program := fixture.New(Options{})
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	ownerRoot := filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")
	connections := ownerinventory.Connections{Root: ownerRoot}
	if err := connections.Write(ownerinventory.Connection{
		HostID: "remote-owner", Destination: "dev@remote.example",
		Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-remote"}},
	}); err != nil {
		t.Fatal(err)
	}
	cache := ownerinventory.Cache{Root: ownerRoot}
	if err := cache.Write(ownerinventory.Snapshot{
		FetchedAt: time.Unix(1, 0).UTC(), Inventory: inventoryResult("remote-owner", "default", "Remote").inventory,
	}); err != nil {
		t.Fatal(err)
	}
	connectionPath := filepath.Join(ownerRoot, "connections", "remote-owner.json")
	cachePath := filepath.Join(ownerRoot, "owners", "remote-owner.json")
	beforeConnection, err := os.ReadFile(connectionPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeCache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}

	results := program.allOwnerInventoriesReadOnly(context.Background(), loaded, false)
	if len(results) != 2 || !results[1].stale || results[1].err == nil ||
		len(results[1].inventory.Yards) != 1 || results[1].inventory.Yards[0].Projects[0].Name != "Remote" {
		t.Fatalf("read-only inventories=%#v", results)
	}
	_, route, err := program.ownerYardRouteReadOnly(context.Background(), loaded, "remote-owner", "default")
	if err != nil || route.SSHHost != "yard-remote" || route.SSHHost == loaded.Context.SSHHost {
		t.Fatalf("remote code route inherited the local alias: route=%#v err=%v", route, err)
	}
	afterConnection, err := os.ReadFile(connectionPath)
	if err != nil {
		t.Fatal(err)
	}
	afterCache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterConnection, beforeConnection) || !bytes.Equal(afterCache, beforeCache) {
		t.Fatal("read-only project resolution rewrote owner inventory state")
	}
}

func TestNamedCodePreservesExplicitRemoteAliasWithoutOwnerRouteWrites(t *testing.T) {
	for _, ownerYard := range []string{"default", "build"} {
		t.Run(ownerYard, func(t *testing.T) {
			fixture := newFixture(t, withEnv("SUBYARD_HOST_ID=local-owner"), withCommands("ssh"))
			root := fixture.Root
			fixture.Exec().
				On(testkit.Match{Tool: "ssh", Prefix: []string{"-G"}}, testkit.Reply{Script: trustedSSHMock(t)}).
				On(testkit.Match{Tool: "ssh", Contains: []string{"-T", "dev@remote.example", "--", "bash", "-lc"}}, testkit.Reply{})
			ownerAssignment := ""
			if ownerYard != "default" {
				ownerAssignment = "OWNER_YARD_NAME=" + ownerYard + "\n"
			}
			fixture.Write("state/yards/preview-remote/config.env",
				"ACCESS_KIND=remote\nOWNER_ENDPOINT=dev@remote.example\nSSH_HOST=yard-preview-remote\n"+ownerAssignment)
			program := fixture.New(Options{})
			loaded, err := program.loadContext("preview-remote")
			if err != nil {
				t.Fatal(err)
			}
			program.env["SUBYARD_YARD_EXPLICIT"], program.env["SUBYARD_YARD"] = "1", loaded.Context.YardName
			ownerRoot := filepath.Join(loaded.Context.Paths.DataHome, "owner-inventory")
			connection := ownerinventory.Connection{HostID: "remote-owner", Destination: "dev@remote.example"}
			connections := ownerinventory.Connections{Root: ownerRoot}
			if err := connections.Write(connection); err != nil {
				t.Fatal(err)
			}
			remote := inventoryResult("remote-owner", ownerYard, "Preview")
			if err := (ownerinventory.Cache{Root: ownerRoot}).Write(ownerinventory.Snapshot{
				FetchedAt: time.Now().UTC(), Inventory: remote.inventory,
			}); err != nil {
				t.Fatal(err)
			}
			protected := []string{filepath.Join(ownerRoot, "connections", "remote-owner.json"), filepath.Join(ownerRoot, "owners", "remote-owner.json")}
			before := make([][]byte, len(protected))
			for index, path := range protected {
				before[index], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			store, err := state.NewFileStore(loaded.Context.Paths.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			record := domain.ProjectRecord{Schema: 1, ProjectID: "preview-id", Name: "Preview", HostPath: "/host/Preview",
				YardPath: state.YardPath("preview-id"), Mode: domain.ProjectSync, SSHHost: loaded.Context.SSHHost, Target: "yard"}
			if err := store.Put(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			match, err := program.resolveOwnerProjectFromInventories(context.Background(), loaded, "Preview", true, true,
				[]ownerInventoryResult{inventoryResult("local-owner", "default", ""), remote})
			if err != nil || match.Yard != "preview-remote" || match.Record != record {
				t.Fatalf("named remote project resolved through a different route: match=%#v err=%v", match, err)
			}
			selected, err := program.activateProjectContext(match.Yard, loaded, true)
			if err != nil || selected.Context != loaded.Context || selected.Environment["SSH_HOST"] != "yard-preview-remote" {
				t.Fatalf("selected alias changed: selected=%#v err=%v", selected.Context, err)
			}
			if err := program.recheckProjectRole(context.Background(), &projectExecution{Loaded: selected, RequiresProjects: true}); err != nil {
				t.Fatalf("role recheck treated the owner yard as a controller registration: %v", err)
			}
			identity, err := canonicalYardIdentity(selected)
			if err != nil || identity != "remote-owner/"+ownerYard {
				t.Fatalf("canonical identity=%q err=%v", identity, err)
			}
			workspaceRoot := filepath.Join(root, "workspaces")
			runner := application.ProjectActionRunner{Data: projectActionObservationProbe{}, Yard: selected.Context,
				Project: record, YardIdentity: identity, WorkspaceDirectory: workspaceRoot}
			if _, _, err := runner.Run(context.Background(), domain.AdapterRequest{Adapter: "project", Action: "code"}, nil); err != nil {
				t.Fatal(err)
			}
			workspace := filepath.Join(workspaceRoot, base64.RawURLEncoding.EncodeToString([]byte(selected.Context.SSHHost))+"."+record.ProjectID, "Preview.code-workspace")
			payload, err := os.ReadFile(workspace)
			var document struct {
				RemoteAuthority string `json:"remoteAuthority"`
			}
			if err != nil || json.Unmarshal(payload, &document) != nil || document.RemoteAuthority != "ssh-remote+yard-preview-remote" {
				t.Fatalf("wrong controller code authority: %q err=%v", payload, err)
			}
			for index, path := range protected {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, before[index]) {
					t.Fatalf("read-only alias routing changed owner evidence: %s err=%v", path, err)
				}
			}
			connection.Yards = map[string]ownerinventory.YardRoute{ownerYard: {SSHHost: "yard-conflicting"}}
			if err := connections.Write(connection); err != nil {
				t.Fatal(err)
			}
			if _, _, err := program.ownerYardRouteReadOnly(context.Background(), loaded, "remote-owner", ownerYard); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("conflicting persisted route was bypassed: %v", err)
			}
		})
	}
}

func TestReadOnlyProjectListHonorsFreshCacheAndLiveForce(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
		wantSSH   bool
	}{
		{name: "default uses fresh cache", arguments: []string{"list"}},
		{name: "yard overview uses fresh cache", arguments: []string{"yards"}},
		{name: "live forces refresh", arguments: []string{"list", "--live"}, wantSSH: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t, withEnv("SUBYARD_HOST_ID=owner-a"), withCommands("ssh", "ssh-keygen"))
			now := time.Unix(1_000, 0).UTC()
			ownerRoot := filepath.Join(fixture.DataHome, "owner-inventory")
			connections := ownerinventory.Connections{Root: ownerRoot}
			if err := connections.Write(ownerinventory.Connection{
				HostID: "remote-owner", Destination: "dev@remote.example",
				Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-remote"}},
			}); err != nil {
				t.Fatal(err)
			}
			cache := ownerinventory.Cache{Root: ownerRoot}
			if err := cache.Write(ownerinventory.Snapshot{
				FetchedAt: now, Inventory: inventoryResult("remote-owner", "default", "Remote").inventory,
			}); err != nil {
				t.Fatal(err)
			}
			cachePath := filepath.Join(ownerRoot, "owners", "remote-owner.json")
			beforeCache, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatal(err)
			}

			liveInventory := inventoryResult("remote-owner", "default", "Live").inventory
			var response bytes.Buffer
			codec := rpc.NewCodec(bytes.NewReader(nil), &response)
			if err := codec.Write(rpc.Response{
				Version: rpc.ProtocolVersion, Type: "response", ID: "negotiate",
				Result: map[string]any{"capabilities": []string{ownerinventory.Capability}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := codec.Write(rpc.Response{
				Version: rpc.ProtocolVersion, Type: "response", ID: "inventory", Result: liveInventory,
			}); err != nil {
				t.Fatal(err)
			}
			reply := fixture.Write("inventory.reply", response.String())
			const knownKey = "remote.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA fixture\n"
			knownHosts := fixture.Write("known_hosts", knownKey)
			lookup := []string{"-F", "remote.example", "-f", knownHosts}
			fixture.Exec().
				On(testkit.Match{Tool: "ssh", Args: []string{"-G", "dev@remote.example"}},
					testkit.Reply{Stdout: "hostname remote.example\nport 22\nuserknownhostsfile " + knownHosts + "\n"}).
				On(testkit.Match{Tool: "ssh-keygen", Args: lookup}, testkit.Reply{Stdout: knownKey}).
				On(testkit.Match{Tool: "ssh", Contains: []string{"dev@remote.example", "--", "bash", "-lc"}},
					testkit.Reply{Script: "exec /bin/cat " + shellquote.Word(reply)})

			var stdout, stderr bytes.Buffer
			program := fixture.New(Options{
				Arguments: test.arguments,
				Stdout:    &stdout, Stderr: &stderr, Clock: testkit.NewManualClock(now),
			})
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("list failed: code=%d stderr=%q", code, stderr.String())
			}
			if test.arguments[0] == "yards" {
				found := false
				for _, line := range strings.Split(stdout.String(), "\n") {
					fields := strings.Fields(line)
					if len(fields) >= 6 && fields[0] == "remote-owner/default" {
						found = fields[5] == "1"
					}
				}
				if !found {
					t.Fatalf("yard overview lost cached project count: %s", stdout.String())
				}
			}
			if test.wantSSH {
				fixture.Exec().AssertCalls(
					testkit.Call{Tool: "ssh", Args: []string{"-G", "dev@remote.example"}},
					testkit.Call{Tool: "ssh-keygen", Args: lookup},
					testkit.Call{Tool: "ssh", Args: []string{
						"-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no",
						"-o", "ControlMaster=no", "-o", "ControlPath=none",
						"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3",
						"dev@remote.example", "--", "bash", "-lc",
						shellquote.Word("exec 'yard' 'rpc' '--stdio'"),
					}},
				)
			} else {
				fixture.Exec().AssertCalls()
			}
			if test.wantSSH && !strings.Contains(stdout.String(), "Live") {
				t.Fatalf("--live did not use the fetched inventory:\n%s", stdout.String())
			}
			afterCache, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterCache, beforeCache) {
				t.Fatal("read-only list rewrote the owner inventory cache")
			}
		})
	}
}

func TestCanonicalReadOnlyInvocationDoesNotRecoverOwnerRoute(t *testing.T) {
	fixture := newFixture(t, withEnv("SUBYARD_HOST_ID=owner-a"))
	environment := fixture.Environment
	ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
	if err := (ownerinventory.Connections{Root: ownerRoot}).Write(ownerinventory.Connection{
		HostID: "remote-owner", Destination: "dev@remote.example",
		Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-remote"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := (ownerinventory.Cache{Root: ownerRoot}).Write(ownerinventory.Snapshot{
		FetchedAt: time.Now().UTC(), Inventory: inventoryResult("remote-owner", "default", "").inventory,
	}); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(ownerRoot, "registration.json")
	journal := []byte("{invalid-owner-recovery\n")
	writeCLIFile(t, journalPath, string(journal), 0o600)

	var stdout, stderr bytes.Buffer
	program := fixture.New(Options{
		Arguments: []string{"-Y", "remote-owner/default", "space", "--help"},
		Stdout:    &stdout, Stderr: &stderr,
	})
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("canonical read failed: code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: yard space") {
		t.Fatalf("canonical read output=%q", stdout.String())
	}
	got, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, journal) {
		t.Fatalf("canonical read changed owner route journal: got %q, want %q", got, journal)
	}
}

func TestRemoteYardStatusAllowsFullProfileChecksOnBothRoutes(t *testing.T) {
	fixture := newFixture(t, withCommands("ssh"))
	root := fixture.Root
	var response bytes.Buffer
	codec := rpc.NewCodec(bytes.NewReader(nil), &response)
	for _, frame := range []rpc.Response{
		{Version: rpc.ProtocolVersion, Type: "response", ID: "negotiate", Result: map[string]any{"capabilities": []string{ownerinventory.ProfileDashboardStatusCapability}}},
		{Version: rpc.ProtocolVersion, Type: "response", ID: "status", Result: domain.YardStatus{State: "RUNNING", Facts: domain.StatusFacts{Shared: []domain.SharedResourceStatus{{Profile: "sample", Name: "dashboard", State: "up", URL: "http://127.0.0.1:22222/"}}}}},
	} {
		if err := codec.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	reply := fixture.Write("status.reply", response.String())
	fixture.Exec().
		On(testkit.Match{Tool: "ssh", Prefix: []string{"-G"}}, testkit.Reply{Script: trustedSSHMock(t)}).
		On(testkit.Match{Tool: "ssh", Contains: []string{"dev@remote.example", "--", "bash", "-lc"}},
			testkit.Reply{Script: "sleep 9\nexec /bin/cat " + shellquote.Word(reply)})
	loaded := config.Loaded{Context: domain.Context{Paths: domain.RuntimePaths{DataHome: root}}}
	if err := (ownerinventory.Connections{Root: filepath.Join(root, "owner-inventory")}).Write(ownerinventory.Connection{HostID: "remote-owner", Destination: "dev@remote.example"}); err != nil {
		t.Fatal(err)
	}
	program := &CLI{}
	for _, route := range []string{"selector", "registered-owner"} {
		t.Run(route, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var status domain.YardStatus
			var err error
			if route == "selector" {
				status, err = program.remoteYardStatus(ctx, domain.Context{OwnerEndpoint: "dev@remote.example", OwnerYardName: "demo"})
			} else {
				var destination string
				status, destination, err = program.remoteOwnerYardStatus(ctx, loaded, "remote-owner", "demo")
				if err == nil && destination != "dev@remote.example" {
					t.Fatal("registered route changed")
				}
			}
			if err != nil || status.State != "RUNNING" || len(status.Facts.Shared) != 1 || status.Facts.Shared[0].State != "up" {
				t.Fatalf("full owner check did not survive transport: status=%+v err=%v", status, err)
			}
		})
	}
}

func TestRPCOwnerInventoryPreservesLegacyProjectState(t *testing.T) {
	fixture := newFixture(t)
	stateDirectory := filepath.Join(fixture.ConfigHome, "projects")
	store, err := state.NewFileStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	record := projectRemovalRecord(domain.ProjectSync)
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDirectory, record.ProjectID+".json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	program := fixture.New(Options{
		Incus: lifecycleIncus(),
	})
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
	result, err := handler.Handle(context.Background(), rpc.Call{
		Method: "owner.inventory", Params: json.RawMessage(`{}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	inventory := result.(domain.OwnerInventory)
	if len(inventory.Yards) != 1 || len(inventory.Yards[0].Projects) != 1 ||
		inventory.Yards[0].Projects[0].ProjectID != record.ProjectID {
		t.Fatalf("owner inventory=%#v", inventory)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || beforeInfo.Mode().Perm() != afterInfo.Mode().Perm() {
		t.Fatalf("owner.inventory mutated legacy state: before=%#o after=%#o",
			beforeInfo.Mode().Perm(), afterInfo.Mode().Perm())
	}
}

func TestProjectListOwnerKeepsYardColumnStable(t *testing.T) {
	for _, owner := range []string{
		"owner-a",
		"5034c950-74d0-46c4-9428-b7835e602109",
	} {
		var output bytes.Buffer
		printProjectListRow(&output, "Demo", "sync", "yard", owner, "default")
		line := strings.TrimSuffix(output.String(), "\n")
		if got, want := strings.Index(line, "default"), 64; got != want {
			t.Fatalf("YARD column for owner %q starts at %d, want %d:\n%s", owner, got, want, line)
		}
	}
}

func TestOwnerYardSelectorRequiresCanonicalPathWhenAmbiguous(t *testing.T) {
	results := []ownerInventoryResult{
		inventoryResult("owner-a", "dev", "Demo"),
		inventoryResult("owner-b", "dev", "Other"),
	}
	if _, _, err := selectOwnerYards(results, "dev"); err == nil ||
		!strings.Contains(err.Error(), "owner-a/dev") ||
		!strings.Contains(err.Error(), "owner-b/dev") {
		t.Fatalf("ambiguous short selector diagnostic drifted: %v", err)
	}
	selected, _, err := selectOwnerYards(results, "owner-b/dev")
	if err != nil || len(selected) != 1 || selected[0].inventory.HostID != "owner-b" {
		t.Fatalf("canonical selector failed: selected=%#v err=%v", selected, err)
	}
}

func TestOwnerIdentityOutputsKeepFullHostID(t *testing.T) {
	const ownerA = "5034c950-74d0-46c4-9428-b7835e602109"
	const ownerB = "6034c950-74d0-46c4-9428-b7835e602109"
	results := []ownerInventoryResult{
		inventoryResult(ownerA, "dev", "Demo"),
		inventoryResult(ownerB, "dev", "Other"),
	}
	var completions bytes.Buffer
	printOwnerCompletions(&completions, results[:1], "projects")
	if !strings.Contains(completions.String(), "Demo/dev/"+ownerA) ||
		strings.Contains(completions.String(), "Demo/dev/"+compactProjectListOwner(ownerA)) {
		t.Fatalf("completion truncated owner identity:\n%s", completions.String())
	}
	if _, _, err := selectOwnerYards(results, "dev"); err == nil ||
		!strings.Contains(err.Error(), ownerA+"/dev") ||
		!strings.Contains(err.Error(), ownerB+"/dev") {
		t.Fatalf("diagnostic truncated owner identity: %v", err)
	}
}

func TestOwnerCompletionPrintsFullAndOnlyUniqueShortSelectors(t *testing.T) {
	results := []ownerInventoryResult{
		inventoryResult("owner-a", "dev", "Demo"),
		inventoryResult("owner-b", "dev", "Demo"),
		inventoryResult("owner-b", "ops", "Unique"),
	}
	var yards bytes.Buffer
	printOwnerCompletions(&yards, results, "yards")
	yardLines := "\n" + yards.String()
	if strings.Contains(yardLines, "\ndev\n") ||
		!strings.Contains(yards.String(), "owner-a/dev") ||
		!strings.Contains(yardLines, "\nops\n") {
		t.Fatalf("yard completion drifted:\n%s", yards.String())
	}
	var projects bytes.Buffer
	printOwnerCompletions(&projects, results, "projects")
	projectLines := "\n" + projects.String()
	if strings.Contains(projectLines, "\nDemo\n") ||
		!strings.Contains(projects.String(), "Demo/dev/owner-a") ||
		!strings.Contains(projectLines, "\nUnique\n") {
		t.Fatalf("project completion drifted:\n%s", projects.String())
	}
}

func TestCanonicalProjectSelectorKeepsProjectPrefix(t *testing.T) {
	if got := canonicalProjectSelector("Demo", "default", "owner-a"); got != "Demo/owner-a" {
		t.Fatalf("default selector = %q", got)
	}
	if got := canonicalProjectSelector("Demo", "dev", "owner-a"); got != "Demo/dev/owner-a" {
		t.Fatalf("named-yard selector = %q", got)
	}
	for _, selector := range []string{
		"Demo/dev/owner-a", "dev/Demo", "owner-a/dev/Demo",
	} {
		if !projectSelectorMatches(selector, "Demo", "dev", "owner-a", true) {
			t.Fatalf("compatible selector %q did not match", selector)
		}
	}
}

// Own the registry-only matrix separately from the remote process/transfer fixture.
func TestOwnerProjectStateConvergesWithoutLosingLocalSource(t *testing.T) {
	for _, yard := range []string{"default", "inner"} {
		t.Run(yard, func(t *testing.T) {
			fixture := newFixture(t, withWorkingDir(""))
			root, directory := fixture.Root, filepath.Join(fixture.ConfigHome, "projects")
			if yard != "default" {
				fixture.Write("state/yards/inner.env", "SSH_PORT=3333\n")
				directory = filepath.Join(root, "state/yards/inner/projects")
			}
			var stdout, stderr bytes.Buffer
			program := fixture.New(Options{Stdout: &stdout, Stderr: &stderr})
			loaded, err := program.loadContext(yard)
			if err != nil {
				t.Fatal(err)
			}
			store, err := state.NewFileStore(directory)
			if err != nil {
				t.Fatal(err)
			}
			call := func(args ...string) int {
				return program.runOwnerProjectState(context.Background(), state.Service{Store: store}, loaded.Context, args)
			}
			id := "demo-12345678"
			if call("upsert", id, "Demo", "sync", "sample") != 0 {
				t.Fatal(stderr.String())
			}
			record, err := store.GetReadOnly(context.Background(), id)
			if err != nil || record.HostPath != "" || record.RegistrySource != "yard" || record.Target != "sample" || record.Mode != domain.ProjectSync || record.YardPath != state.YardPath(id) || record.SSHHost != loaded.Context.SSHHost {
				t.Fatalf("owner record=%#v %v", record, err)
			}
			record.HostPath = "/owner/Demo"
			if err := store.Put(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			if call("upsert", id, "Demo", "git", "yard") != 0 || call("unregister", id) != 0 {
				t.Fatal(stderr.String())
			}
			record, err = store.GetReadOnly(context.Background(), id)
			if err != nil || record.HostPath != "/owner/Demo" || record.Mode != domain.ProjectGit || record.Target != "yard" || record.RegistrySource != "" {
				t.Fatalf("refreshed owner=%#v %v", record, err)
			}
			before := nativeTreeSnapshot(t, directory)
			if call("remove", id, state.SourceKey("/wrong/source")) == 0 || strings.Join(before, "\n") != strings.Join(nativeTreeSnapshot(t, directory), "\n") {
				t.Fatal("mismatched removal changed owner registry")
			}
			if call("remove", id, state.SourceKey(record.HostPath)) != 0 {
				t.Fatal(stderr.String())
			}
			if _, err := store.GetReadOnly(context.Background(), id); !errors.Is(err, state.ErrNotFound) {
				t.Fatal("source-bound removal retained owner record", err)
			}
			if call("upsert", id, "Demo", "sync", "yard") != 0 || call("unregister", id) != 0 {
				t.Fatal(stderr.String())
			}
			if _, err := store.GetReadOnly(context.Background(), id); !errors.Is(err, state.ErrNotFound) {
				t.Fatal("unregister retained synthetic record", err)
			}
			before = nativeTreeSnapshot(t, root)
			if call("upsert", "../escape", "Bad", "sync", "yard") == 0 || strings.Join(before, "\n") != strings.Join(nativeTreeSnapshot(t, root), "\n") {
				t.Fatal("unsafe ID mutated filesystem")
			}
		})
	}
}
