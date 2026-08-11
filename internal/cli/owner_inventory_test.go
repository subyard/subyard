package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/state"
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
				"default":  {SSHHost: "yard-remote"},
				"openclaw": {SSHHost: "yard-remote-openclaw"},
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
			yard: domain.Context{YardName: "default", YardType: domain.YardLocal},
			want: "local-owner/default",
		},
		{
			name: "local named",
			yard: domain.Context{YardName: "openclaw", YardType: domain.YardLocal},
			want: "local-owner/openclaw",
		},
		{
			name: "remote default explicit",
			yard: domain.Context{
				YardName: "default", YardType: domain.YardRemote,
				RemoteDest: "dev@remote.example", RemoteYard: "default",
			},
			want: "remote-owner/default",
		},
		{
			name: "remote default implicit",
			yard: domain.Context{
				YardName: "local-route-name", YardType: domain.YardRemote,
				RemoteDest: "dev@remote.example",
			},
			want: "remote-owner/default",
		},
		{
			name: "remote named",
			yard: domain.Context{
				YardName: "openclaw", YardType: domain.YardRemote,
				RemoteDest: "dev@remote.example", RemoteYard: "openclaw",
			},
			want: "remote-owner/openclaw",
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
		YardName: "default", YardType: domain.YardRemote, RemoteDest: "dev@missing.example",
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

	results := program.allOwnerInventoriesReadOnly(context.Background(), loaded)
	if len(results) != 2 || !results[1].stale || results[1].err == nil ||
		len(results[1].inventory.Yards) != 1 || results[1].inventory.Yards[0].Projects[0].Name != "Remote" {
		t.Fatalf("read-only inventories=%#v", results)
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
	handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]rpcPlannedOperation)}
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
