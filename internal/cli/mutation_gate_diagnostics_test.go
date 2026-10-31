package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMutationGateRetainsExactInspectionDiagnostics(t *testing.T) {
	for _, transport := range []string{"CLI", "RPC"} {
		t.Run(transport, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			runtimeRoot := filepath.Join(root, "runtime-diagnostics")
			capture := filepath.Join(root, "v2-candidate-inspections")
			environment = append(environment, "YARD_RUNTIME_ROOT="+runtimeRoot, "V2_GATE_CAPTURE="+capture)
			journalPath, _ := installUnfinishedV2MutationGateFixture(t, root, environment, runtimeRoot, func(script string) string {
				script = strings.Replace(script, `"status":"recovering"`, `"status":"operator-action-required"`, 1)
				script = strings.Replace(script, `"code":"recovery-pending"`, `"code":"dependency-unavailable"`, 1)
				script = strings.Replace(script, `"message":"the authorized release transition can resume from observed facts"`, `"message":"fixture dependency is unavailable"`, 1)
				script = strings.Replace(script, `"retry":"run yard update"`, `"retry":"restore fixture dependency"`, 1)
				return strings.Replace(script, `"resume":"%s","outcome"`, `"decisions":[{"resource":"activation.fixture-runtime","scope":"activation","decision":"canonicalize","result":"converged"}],"blockers":[{"code":"dependency-unavailable","resource":"activation.fixture-runtime","message":"fixture dependency is unavailable","retry":"restore fixture dependency"}],"resume":"%s","outcome"`, 1)
			})
			runner := &testkit.ScriptedAdapter{}
			var stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"start"}, Environment: environment, WorkingDir: root, Stderr: &stderr, Incus: lifecycleIncus(), AdapterRunner: runner})
			if err != nil {
				t.Fatal(err)
			}
			var payload []byte
			if transport == "CLI" {
				if code := program.Run(context.Background()); code != 1 {
					t.Fatalf("refusal exit=%d stderr=%q", code, stderr.String())
				}
				payload = bytes.TrimSpace(stderr.Bytes())
			} else {
				loaded, err := program.loadContext("default")
				if err != nil {
					t.Fatal(err)
				}
				handler := &rpcHandler{cli: program, loaded: loaded, plans: make(map[string]*preparedCommand)}
				_, err = handler.Handle(context.Background(), rpc.Call{Method: "operation.plan", OperationID: "diagnostic-refusal", Params: json.RawMessage(`{"command":"start","arguments":[]}`)}, nil)
				fault, ok := err.(*rpc.Error)
				if !ok || fault.Code != "dependency-unavailable" {
					t.Fatalf("RPC refusal=%#v", err)
				}
				payload = []byte(fault.Message)
			}
			var refused mutationGateOutcome
			if err := json.Unmarshal(payload, &refused); err != nil {
				t.Fatalf("refusal JSON=%q: %v", payload, err)
			}
			if refused.Status != releasetransition.StatusOperatorActionRequired || refused.Action != "restore fixture dependency" ||
				len(refused.Decisions) != 1 || refused.Decisions[0].Resource != "activation.fixture-runtime" ||
				len(refused.Blockers) != 1 || refused.Blockers[0].Message != "fixture dependency is unavailable" {
				t.Fatalf("lost same-inspection diagnostics: %#v", refused)
			}
			calls, err := os.ReadFile(capture)
			if err != nil || string(calls) != "called" || len(runner.Requests) != 0 {
				t.Fatalf("refusal reobserved or mutated: calls=%q requests=%v err=%v", calls, runner.Requests, err)
			}
			journalBytes, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := releasetransition.ParseJournal(journalBytes)
			if err != nil {
				t.Fatal(err)
			}
			journal.Checkpoint = releasetransition.JournalComplete
			journalBytes, err = releasetransition.MarshalJournal(journal)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, journalPath, journalBytes, 0600)
			links, err := releasetransition.NewRuntimeLinkStore(runtimeRoot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := links.Activate(journal.Releases); err != nil {
				t.Fatal(err)
			}
			later, err := program.inspectMutationGate(context.Background(), "default")
			if err != nil || later != nil {
				t.Fatalf("later readiness=%#v err=%v", later, err)
			}
			// The already emitted refusal must keep its original resource and reason.
			if !bytes.Contains(payload, []byte("activation.fixture-runtime")) || !bytes.Contains(payload, []byte("fixture dependency is unavailable")) {
				t.Fatalf("later readiness erased refusal: %q", payload)
			}
		})
	}
}

func TestMutationGateOmitsDiagnosticsFromReplacedOutcome(t *testing.T) {
	original := &releasetransition.Outcome{Status: releasetransition.StatusRecovering}
	inspection := &releasetransition.Inspection{Outcome: original, Decisions: []releasetransition.RedactedDecision{{Resource: "activation.original"}}}
	replaced := *original
	rendered := publicInspectedMutationGateOutcome(&replaced, inspection)
	if len(rendered.Decisions) != 0 || len(rendered.Blockers) != 0 {
		t.Fatalf("unrelated observation diagnostics retained: %#v", rendered)
	}
}
