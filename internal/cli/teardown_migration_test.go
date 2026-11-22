package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func TestFullUninstallDoesNotRequireMigration(t *testing.T) {
	for _, state := range []string{"V1 pending", "V1 invalid", "V2 pending", "V2 invalid"} {
		for _, invocation := range []struct {
			name        string
			arguments   []string
			input       string
			terminal    bool
			fullRemoval bool
		}{
			{name: "explicit full", arguments: []string{"teardown", "--full"}, fullRemoval: true},
			{name: "uninstall alias", arguments: []string{"uninstall", "--full"}, fullRemoval: true},
			{name: "interactive full", arguments: []string{"uninstall"}, input: "2\n", terminal: true, fullRemoval: true},
			{name: "selected yard", arguments: []string{"teardown", "--yes"}},
			{name: "start", arguments: []string{"start"}},
		} {
			t.Run(state+"/"+invocation.name, func(t *testing.T) {
				fixture := newFixture(t)
				root, environment := fixture.Root, fixture.Environment
				registryPath := filepath.Join(root, "config", "commands.registry")
				registry, err := os.ReadFile(registryPath)
				if err != nil {
					t.Fatal(err)
				}
				fixture.Write(registryPath, string(bytes.Replace(registry, []byte("teardown||"), []byte("teardown|uninstall|"), 1)))
				runtimeRoot := filepath.Join(root, "runtime")
				capture := filepath.Join(root, "v2-candidate-inspections")
				environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "V2_GATE_CAPTURE="+capture)
				configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
				var journal string
				if strings.HasPrefix(state, "V1") {
					id := installUnfinishedMutationGateFixture(t, root, environment, runtimeRoot)
					journal = filepath.Join(configHome, "migrations", "transactions", id, "transaction.json")
				} else {
					journal, _ = installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
				}
				if strings.HasSuffix(state, "invalid") {
					fixture.Write(journal, "{", 0600)
				}
				before, err := os.ReadFile(journal)
				if err != nil {
					t.Fatal(err)
				}
				var migrationsBefore []string
				if strings.HasPrefix(state, "V1") {
					fixture.Write(filepath.Join(configHome, "migrations", "state.json"), "retained V1 state\n", 0600)
					migrationsBefore = nativeTreeSnapshot(t, filepath.Join(configHome, "migrations"))
				}
				installation := uninstall.Installation{Schema: 1, OperatorUID: uint32(os.Getuid()),
					DataHome: environmentValue(environment, "SUBYARD_HOME"), ConfigHome: configHome,
					RuntimeRoot: runtimeRoot, CacheRoot: filepath.Join(root, "cache")}
				if err := os.MkdirAll(installation.DataHome, 0700); err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(installation)
				if err != nil {
					t.Fatal(err)
				}
				fixture.Write(filepath.Join(installation.DataHome, uninstall.ManifestName), string(body), 0600)
				var stderr bytes.Buffer
				prompt := &testkit.Prompt{Answers: []bool{false}}
				observed := false
				cleanupBlocked := errors.New("fixture cleanup inventory unavailable")
				native := lifecycleIncus()
				var adapter ports.AdapterRunner
				var runner *testkit.ScriptedAdapter
				if strings.HasPrefix(state, "V1") && !invocation.fullRemoval {
					environment = append(environment, "SUBYARD_OPERATION_ID=v1-admission")
					runner = &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
						Schema: 1, OperationID: "v1-admission", Status: "ok",
					}}}}
					adapter = runner
					if invocation.name == "start" {
						native = powerScriptedIncus(runner, native)
					} else {
						native.Reconcile.InstanceFound = true
						runner.Steps[0].Apply = func(domain.AdapterRequest) {
							native.Reconcile.InstanceFound = false
							native.Instances = map[string]ports.InstanceInfo{}
						}
					}
				}
				program := fixture.New(Options{Arguments: invocation.arguments,
					Environment: environment, Incus: native, AdapterRunner: adapter, NetworkPolicy: allowTestNetworkPolicy(), Prompt: prompt,
					Stdin: strings.NewReader(invocation.input), Stdout: io.Discard, Stderr: &stderr,
					ObserveCoreProducers: func(config.Loaded, uninstall.RecoveryPlan, []string, int) ([]uninstall.RecoveryService, error) {
						observed = true
						if invocation.fullRemoval {
							return nil, cleanupBlocked
						}
						return nil, nil
					},
				})
				program.promptInputTerminal = func() bool { return invocation.terminal }
				code := program.Run(context.Background())
				if (invocation.fullRemoval || strings.HasPrefix(state, "V2")) && (code == 0 || observed != invocation.fullRemoval || len(prompt.Requests) != 0) {
					t.Fatalf("cleanup admission: code=%d observed=%t prompts=%d stderr=%s", code, observed, len(prompt.Requests), stderr.String())
				}
				if invocation.fullRemoval {
					if !strings.Contains(stderr.String(), cleanupBlocked.Error()) {
						t.Fatalf("full uninstall bypassed its cleanup guard: %s", stderr.String())
					}
					if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("full uninstall inspected the migration owner: %v", err)
					}
				} else if strings.HasPrefix(state, "V2") {
					outcome, err := decodeGateRefusal(stderr.Bytes())
					if err != nil ||
						(outcome.Status != "recovering" && outcome.Status != "operator-action-required") {
						t.Fatalf("ordinary mutation bypassed migration gate: %s, %v", stderr.String(), err)
					}
				}
				if strings.HasPrefix(state, "V1") && !invocation.fullRemoval {
					if code != 0 || observed != (invocation.name == "selected yard") || len(prompt.Requests) != 0 || len(runner.Requests) != 1 {
						t.Fatalf("V1 files blocked ordinary command: code=%d observed=%t prompts=%d calls=%d stderr=%s", code, observed, len(prompt.Requests), len(runner.Requests), &stderr)
					}
					if !slices.Equal(migrationsBefore, nativeTreeSnapshot(t, filepath.Join(configHome, "migrations"))) {
						t.Fatal("admitted command changed retained migrations directory")
					}
				}

				after, err := os.ReadFile(journal)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("admission changed migration state: %v", err)
				}
			})
		}
	}
}

func TestCanonicalTeardownMutationGatePreservesOwnerInventory(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		name := "pending"
		if invalid {
			name = "invalid"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t)
			root, environment := fixture.Root, fixture.Environment
			runtimeRoot := filepath.Join(root, "runtime")
			environment = append(environment, "SUBYARD_HOST_ID=owner-a", "YARD_RUNTIME_ROOT="+runtimeRoot)
			journal, _ := installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot)
			if invalid {
				fixture.Write(journal, "{", 0600)
			}
			ownerRoot := filepath.Join(environmentValue(environment, "SUBYARD_HOME"), "owner-inventory")
			if err := os.MkdirAll(ownerRoot, 0700); err != nil {
				t.Fatal(err)
			}
			pending, err := json.Marshal(map[string]any{
				"schemaVersion": 1,
				"connection":    ownerinventory.Connection{HostID: "pending-owner", Destination: "dev@remote.example"},
				"snapshot": ownerinventory.Snapshot{FetchedAt: time.Now().UTC(),
					Inventory: inventoryResult("pending-owner", "default", "").inventory},
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.Write(filepath.Join(ownerRoot, "registration.json"), string(pending), 0600)
			before := nativeTreeSnapshot(t, ownerRoot)
			var stderr bytes.Buffer
			program := fixture.New(Options{
				Arguments:   []string{"-Y", "owner-a/default", "teardown", "--yes"},
				Environment: environment, Incus: lifecycleIncus(),
				Stdout: io.Discard, Stderr: &stderr})
			if code := program.Run(context.Background()); code != 1 {
				t.Fatalf("canonical teardown bypassed migration gate: code=%d stderr=%s", code, stderr.String())
			}
			outcome, err := decodeGateRefusal(stderr.Bytes())
			if err != nil || outcome.Code == "" {
				t.Fatalf("canonical teardown lost migration diagnostic: %s, %v", stderr.String(), err)
			}
			if !slices.Equal(before, nativeTreeSnapshot(t, ownerRoot)) {
				t.Fatal("blocked teardown recovered or rewrote owner inventory")
			}
		})
	}
}
