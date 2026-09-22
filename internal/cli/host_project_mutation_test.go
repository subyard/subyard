package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
)

type hostMutationPrompt func(context.Context, domain.ConfirmationRequest) (bool, error)

func (prompt hostMutationPrompt) Confirm(ctx context.Context, request domain.ConfirmationRequest) (bool, error) {
	return prompt(ctx, request)
}

func TestHostRemoveRefreshesInventoryAfterConfirmation(t *testing.T) {
	root, home, configHome, environment := configCommandFixture(t)
	writeConfigCommandFile(t, configsync.HostIDPath(configHome), "local-owner\n", 0o600)
	empty := inventoryResult("owner-a", "default", "").inventory
	_, trust := hostAddSSHFixture(t, empty)
	store := ownerinventory.Connections{Root: filepath.Join(home, ".subyard", "owner-inventory")}
	connection := ownerinventory.Connection{HostID: "owner-a", Destination: "owner-alias", Trust: &trust}
	if err := store.Register(connection, ownerinventory.Snapshot{FetchedAt: time.Now(), Inventory: empty}); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	prompt := hostMutationPrompt(func(context.Context, domain.ConfirmationRequest) (bool, error) {
		prompts++
		writeHostRPCFixture(t, os.Getenv("SSH_RPC_RESPONSE"), inventoryResult("owner-a", "default", "NewProject").inventory)
		return true, nil
	})
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"host", "remove", "owner-a"},
		Environment: environment, WorkingDir: root, Stdout: io.Discard, Stderr: &stderr, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 1 || prompts != 1 || !strings.Contains(stderr.String(), "project reference") {
		t.Fatalf("removal used pre-confirmation inventory: code=%d prompts=%d stderr=%s", code, prompts, stderr.String())
	}
	if records, err := store.List(); err != nil || len(records) != 1 {
		t.Fatalf("removal lost registration: %#v, %v", records, err)
	}
}

func TestPreparedProjectRejectsRemovedOwnerBeforeRefreshOrPhysicalWork(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	store := ownerinventory.Connections{Root: filepath.Join(data, "owner-inventory")}
	connection := ownerinventory.Connection{HostID: "owner-a", Destination: "owner-alias",
		Yards: map[string]ownerinventory.YardRoute{"default": {SSHHost: "yard-alias"}}}
	snapshot := ownerinventory.Snapshot{FetchedAt: time.Now(), Inventory: inventoryResult("owner-a", "default", "").inventory}
	_, trust := hostAddSSHFixture(t, snapshot.Inventory)
	connection.Trust = &trust
	if err := store.Register(connection, snapshot); err != nil {
		t.Fatal(err)
	}
	routing := filepath.Join(store.Root, "routing", connection.HostID)
	yard := domain.Context{AccessKind: domain.AccessRemote, OwnerEndpoint: connection.Destination,
		OwnerYardName: "default", SSHHost: "yard-alias"}
	yard.Paths.DataHome = data
	yard.Paths.StateDir = filepath.Join(routing, "default", "projects")
	project := &projectExecution{Loaded: config.Loaded{Context: yard}}
	cli := &CLI{}
	if err := cli.captureProjectOwner(project); err != nil {
		t.Fatal(err)
	}
	if _, err := openProjectPreparationStore(ctx, yard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(routing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparation created routing state: %v", err)
	}
	plan, err := store.PrepareRemoval(connection, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyRemoval(ctx, plan, func(context.Context, ownerinventory.Connection) (ownerinventory.Snapshot, error) {
		return snapshot, nil
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	prepared := &preparedCommand{CLI: cli, Project: project,
		Plan: domain.OperationPlan{Confirmed: true, Assessment: &domain.ActionAssessment{Action: "project.sync", Changed: true, Effect: domain.ActionMutation}},
		refresh: func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
			called = true
			return "project.sync", domain.ActionDelta{Changed: true}, nil
		},
		execute: func(context.Context, *application.Orchestrator, io.Writer) (domain.AdapterResult, error) {
			called = true
			return domain.AdapterResult{Status: "ok"}, nil
		},
	}
	if _, err := prepared.Execute(ctx, &application.Orchestrator{}, io.Discard); err == nil || called {
		t.Fatalf("removed owner reached project execution: called=%v err=%v", called, err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(routing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale command recreated routing state: %v", err)
	}
	if err := cli.captureProjectOwner(&projectExecution{Loaded: config.Loaded{Context: yard}}); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("removed canonical route still prepares: %v", err)
	}
}
