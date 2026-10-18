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

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestStatusReportsInstalledReleaseReadiness(t *testing.T) {
	for _, view := range []string{"summary", "detail"} {
		for _, state := range []string{"ready", "drift", "unavailable"} {
			t.Run(view+"/"+state, func(t *testing.T) {
				root, environment, marker := migrateCLIFixture(t, state == "drift")
				if state == "unavailable" {
					path := filepath.Join(environmentValue(environment, "YARD_RUNTIME_ROOT"), "current", "bin", "yard-engine")
					testkit.WriteFile(t, path, []byte("private inspection failure\n"), 0o700)
				}
				arguments := []string{"status"}
				if view == "detail" {
					arguments = []string{"-Y", "default", "status"}
				}
				var stdout, stderr bytes.Buffer
				prompt := &testkit.Prompt{}
				incus := lifecycleIncus()
				program, err := New(Options{
					RepositoryRoot: root, Program: "yard", Arguments: arguments,
					Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr,
					Incus: incus, Executor: incus, StatusFacts: statusFactsStub{}, Prompt: prompt,
				})
				if err != nil {
					t.Fatal(err)
				}
				if code := program.Run(context.Background()); code != 0 {
					t.Fatalf("status code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				want := "release  OK (ready)"
				if state == "drift" {
					want = "release  NOT OK (migration-required)"
					if !strings.Contains(stdout.String(), "runtime drift") || !strings.Contains(stdout.String(), "next: yard migrate") {
						t.Fatalf("missing repair diagnostic: %s", stdout.String())
					}
				}
				if state == "unavailable" {
					want = "release  unknown"
				}
				if !strings.Contains(stdout.String(), want) || !strings.Contains(stdout.String(), "projects") ||
					strings.Contains(stdout.String(), "private inspection failure") || stderr.Len() != 0 {
					t.Fatalf("status stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), want)
				}
				if len(prompt.Requests) != 0 || len(incus.PowerUpdates) != 0 {
					t.Fatal("readiness observation prompted or changed yard power")
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("status executed migration: %v", err)
				}
			})
		}
	}
}

func TestStatusReleaseReadinessPreservesUnfinishedTransition(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runtimeRoot := filepath.Join(root, "runtime-v2")
	environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "V2_GATE_CAPTURE="+filepath.Join(root, "capture"))
	journal, _ := installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"status"},
		Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Incus: lifecycleIncus()})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(context.Background()); code != 0 || !strings.Contains(stdout.String(), "release  NOT OK") {
		t.Fatalf("recovery status code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("status changed protected transition: %v", err)
	}
}

func TestReleaseReadinessRPCAndCancellation(t *testing.T) {
	root, environment, _ := migrateCLIFixture(t, false)
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcHandler{cli: program}
	result, err := handler.Handle(context.Background(), rpc.Call{Method: "release.readiness", Params: json.RawMessage(`{}`)}, nil)
	if err != nil || result.(*domain.ReleaseReadiness).Status != string(releasetransition.StatusReady) {
		t.Fatalf("readiness RPC result=%#v err=%v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if readiness := program.releaseReadiness(ctx); readiness.Status != "unknown" {
		t.Fatalf("cancelled observation claimed readiness: %+v", readiness)
	}
}

func TestStatusReportsRemoteReleaseReadinessSeparatelyFromCachedInventory(t *testing.T) {
	for _, view := range []string{"summary", "detail"} {
		for _, state := range []string{"recovering", "old-owner"} {
			t.Run(view+"/"+state, func(t *testing.T) {
				root, environment, _ := migrateCLIFixture(t, false)
				environment = append(environment, "SUBYARD_HOST_ID=local-owner")
				inventory := inventoryResult("remote-owner", "default", "Remote").inventory
				_, trust := hostAddSSHFixture(t, inventory)
				if state == "old-owner" {
					writeStatusOwnerResponses(t, inventory)
				} else {
					writeStatusOwnerResponses(t, inventory, &domain.ReleaseReadiness{
						Status: "recovering", Code: "recovery-pending", Message: "an update is unfinished", Next: "yard update",
					})
				}
				store := ownerinventory.Connections{Root: filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")}
				connection := ownerinventory.Connection{HostID: inventory.HostID, Destination: "owner-alias", Trust: &trust}
				if err := store.Register(connection, ownerinventory.Snapshot{FetchedAt: time.Now().UTC(), Inventory: inventory}); err != nil {
					t.Fatal(err)
				}
				arguments := []string{"status"}
				if view == "detail" {
					arguments = []string{"-Y", "remote-owner/default", "status"}
				}
				var stdout, stderr bytes.Buffer
				program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: arguments,
					Environment: environment, WorkingDir: root, Stdout: &stdout, Stderr: &stderr, Incus: lifecycleIncus()})
				if err != nil {
					t.Fatal(err)
				}
				if code := program.Run(context.Background()); code != 0 {
					t.Fatalf("remote status code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				_, remote, found := strings.Cut(stdout.String(), "remote-owner/default")
				want := "release  NOT OK (recovering)"
				if state == "old-owner" {
					want = "release  unknown"
				}
				if !found || !strings.Contains(remote, want) || strings.Contains(remote, "release  OK") ||
					!strings.Contains(remote, "next on owner ") || stderr.Len() != 0 {
					t.Fatalf("remote status used local/cached readiness: stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
			})
		}
	}
}
