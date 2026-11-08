package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestCompletedPendingOwnershipSelectionReductionPreservesHistory(t *testing.T) {
	if os.Getenv("SUBYARD_SOURCE_QUERY_TOOLING_ROOT") != "" {
		t.Skip("typed process-protocol fixture; physical owner covered by Orca recovery lane")
	}
	for _, scenario := range []string{"valid", "corrupt-history"} {
		t.Run(scenario, func(t *testing.T) {
			program, _, home := completedSourceAuthoringFixture(t, false, map[string]string{"config/fixture-pending-ownership": "fixture\n"})
			path := filepath.Join(home, "yards/default/config.env")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			before := []byte("# preserve\nCODING_TOOL_INTEGRATIONS='codex claude'\n")
			testkit.WriteFile(t, path, before, 0600)
			if scenario == "corrupt-history" {
				testkit.WriteFile(t, filepath.Join(home, "release-transition/v2/history-checkpoint.json"), []byte("{}\n"), 0600)
			}
			if scenario == "valid" {
				outcome, inspection, err := program.inspectMutationGateInspection(context.Background(), "default")
				if err != nil || !program.pendingIntegrationConfigurationGate(outcome, inspection) {
					t.Fatalf("fixture is not typed pending ownership: outcome=%#v inspection=%#v err=%v", outcome, inspection, err)
				}
			}
			history := activationRepairHistory(t, program.options.RepositoryRoot, home)
			options := program.options
			options.Arguments = []string{"config", "set", "CODING_TOOL_INTEGRATIONS", "codex", "--scope", "yard", "--local", "--yes"}
			var output bytes.Buffer
			options.Stdout = &output
			options.Stderr = &output
			code := runAliasRepairFixture(t, options)
			if scenario == "corrupt-history" {
				if code == 0 || !bytes.Equal(before, mustReadConfigFixture(t, path)) {
					t.Fatalf("corrupt source history admitted: %d %s", code, output.String())
				}
			} else {
				if code != 0 {
					t.Fatalf("pending selection rollback: %d %s", code, output.String())
				}
				loaded, err := program.loadSelectionRepair("default")
				if err != nil || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "codex" {
					t.Fatalf("selection not restored %v", err)
				}
				outcome, inspection, err := program.inspectMutationGateInspection(context.Background(), "default")
				if err != nil || !program.pendingIntegrationConfigurationGate(outcome, inspection) {
					t.Fatalf("readiness was hidden: %#v %v", outcome, err)
				}
			}
			if !reflect.DeepEqual(history, activationRepairHistory(t, program.options.RepositoryRoot, home)) {
				t.Fatal("source-only rollback changed release history")
			}
			if scenario == "valid" && strings.Contains(output.String(), "reduce-selection") {
				t.Fatal("completed source authoring used interrupted-history fallback")
			}
		})
	}
}

func TestPendingConfigurationGateRequiresOnlyTypedCompletedReadiness(t *testing.T) {
	for _, scenario := range []string{"pending", "history", "untyped", "resuming", "different-target", "different-inspection", "no-blockers"} {
		t.Run(scenario, func(t *testing.T) {
			outcome := &releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired, Code: releasetransition.CodeDependencyUnavailable, Active: "0.26.0-fixture", Target: "0.26.0-fixture"}
			inspection := &releasetransition.Inspection{Outcome: outcome, Blockers: []releasetransition.Blocker{{Code: releasetransition.CodeDependencyUnavailable, Resource: "activation.materialized-config.pending-configuration"}}}
			tx := releasetransition.TransactionID("tx-fixture")
			switch scenario {
			case "history":
				inspection.Blockers = append(inspection.Blockers, releasetransition.Blocker{Code: releasetransition.CodeDependencyUnavailable, Resource: "transition.source-metadata"})
			case "untyped":
				inspection.Blockers[0].Resource = "activation.materialized-config"
			case "resuming":
				inspection.Resume = &tx
			case "different-target":
				outcome.Target = "0.27.0-fixture"
			case "different-inspection":
				copy := *outcome
				inspection.Outcome = &copy
			case "no-blockers":
				inspection.Blockers = nil
			}
			if got := pendingIntegrationConfigurationShape(outcome, inspection); got != (scenario == "pending") {
				t.Fatalf("pending admission=%t", got)
			}
		})
	}
	if !ordinaryCommandDuringPendingConfiguration("@ssh-agent") || !ordinaryCommandDuringPendingConfiguration("@project") {
		t.Fatal("ordinary command is blocked")
	}
	for _, handler := range []string{"@init", "@config", "@integration", "@profile", "@update", "@migrate"} {
		if ordinaryCommandDuringPendingConfiguration(handler) {
			t.Fatalf("%s bypasses readiness", handler)
		}
	}
}
