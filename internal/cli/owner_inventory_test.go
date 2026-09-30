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
	root, environment, _ := nativeFixture(t)
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil || route.CodeSSHHost != "yard-remote.code" || route.CodeSSHHost == loaded.Context.CodeSSHHost {
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
			root, environment, _ := nativeFixture(t)
			configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			aliasDirectory := filepath.Join(configHome, "yards", "preview-remote")
			if err := os.MkdirAll(aliasDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			ownerAssignment := ""
			if ownerYard != "default" {
				ownerAssignment = "OWNER_YARD_NAME=" + ownerYard + "\n"
			}
			testkit.WriteFile(t, filepath.Join(aliasDirectory, "config.env"), []byte(
				"ACCESS_KIND=remote\nOWNER_ENDPOINT=dev@remote.example\nSSH_HOST=yard-preview-remote\n"+ownerAssignment), 0o600)
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: append(environment, "SUBYARD_HOST_ID=local-owner"), WorkingDir: root,
			})
			if err != nil {
				t.Fatal(err)
			}
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
			if err != nil || selected.Context != loaded.Context || selected.Environment["SSH_CODE_HOST"] != "yard-preview-remote.code" {
				t.Fatalf("selected alias changed: selected=%#v err=%v", selected.Context, err)
			}
			if err := program.recheckProjectRole(&projectExecution{Loaded: selected, RequiresProjects: true}); err != nil {
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
			workspace := filepath.Join(workspaceRoot, base64.RawURLEncoding.EncodeToString([]byte(selected.Context.CodeSSHHost))+"."+record.ProjectID, "Preview.code-workspace")
			payload, err := os.ReadFile(workspace)
			var document struct {
				RemoteAuthority string `json:"remoteAuthority"`
			}
			if err != nil || json.Unmarshal(payload, &document) != nil || document.RemoteAuthority != "ssh-remote+yard-preview-remote.code" {
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
		{name: "live forces refresh", arguments: []string{"list", "--live"}, wantSSH: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			now := time.Unix(1_000, 0).UTC()
			ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
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

			bin := filepath.Join(root, "fake-bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			sshLog := filepath.Join(root, "ssh-called")
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
			encodedResponse := base64.StdEncoding.EncodeToString(response.Bytes())
			writeCLIFile(t, filepath.Join(bin, "ssh"),
				"#!/bin/sh\n"+trustedSSHMock(t)+"printf x >> \"$SUBYARD_TEST_SSH_LOG\"\nprintf '%s' '"+encodedResponse+"' | /usr/bin/base64 -d\n",
				0o700,
			)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("SUBYARD_TEST_SSH_LOG", sshLog)

			var stdout, stderr bytes.Buffer
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Arguments: test.arguments,
				Environment: append(environment, "SUBYARD_HOST_ID=owner-a"), WorkingDir: root,
				Stdout: &stdout, Stderr: &stderr, Clock: testkit.NewManualClock(now),
			})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("list failed: code=%d stderr=%q", code, stderr.String())
			}
			_, sshErr := os.Lstat(sshLog)
			if test.wantSSH && sshErr != nil {
				t.Fatalf("--live did not force an owner inventory fetch: %v", sshErr)
			}
			if !test.wantSSH && !errors.Is(sshErr, os.ErrNotExist) {
				t.Fatalf("default list fetched despite a fresh cache: %v", sshErr)
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
	root, environment, _ := nativeFixture(t)
	environment = append(environment, "SUBYARD_HOST_ID=owner-a")
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
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard",
		Arguments:   []string{"-Y", "remote-owner/default", "space", "--help"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
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

func TestRPCOwnerInventoryPreservesLegacyProjectState(t *testing.T) {
	root, environment, stateDirectory := nativeFixture(t)
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
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment, WorkingDir: root,
		Incus: lifecycleIncus(),
	})
	if err != nil {
		t.Fatal(err)
	}
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
