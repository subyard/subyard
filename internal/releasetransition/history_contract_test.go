package releasetransition

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestHistoryCompilerBoundaries(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "subyard-history-contract-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	testkit.WriteFile(t, filepath.Join(dir, "go.mod"), []byte("module github.com/Subyard/Subyard/historycontract\n\ngo 1.25.6\n\nrequire github.com/Subyard/Subyard v0.0.0\nreplace github.com/Subyard/Subyard => "+root+"\n"), 0o600)
	fixtures := map[string]string{
		"positive": `package contract
import mh "github.com/Subyard/Subyard/internal/migrationhistory"
func next(r mh.Registry, a mh.Authority) (mh.Edge,error) { return r.Next(a) }
`,
		"projection": `package contract
import mh "github.com/Subyard/Subyard/internal/migrationhistory"
func invalid(r mh.Registry,p mh.CompatibilityProjection) { r.Next(p) }
`,
		"digest": `package contract
import mh "github.com/Subyard/Subyard/internal/migrationhistory"
func invalid(r mh.Registry,p string) { r.Next(p) }
`,
	}
	for name, source := range fixtures {
		t.Run(name, func(t *testing.T) {
			if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, name, "contract.go")
			testkit.WriteFile(t, path, []byte(source), 0o600)
			cmd := exec.Command("go", "test", "-mod=mod", "./"+name)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "TMPDIR=/tmp", "GOWORK=off")
			output, err := cmd.CombinedOutput()
			if name == "positive" {
				if err != nil {
					t.Fatalf("positive contract: %v\n%s", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(string(output), "cannot use p") {
				t.Fatalf("compiler did not reject forbidden composition: %v\n%s", err, output)
			}
		})
	}
}

// Overlay an existing same-package test file so private production consumers
// remain private. No fixture is written into the managed checkout.
func TestV2PhaseCompilerBoundaries(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "subyard-phase-contract-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cases := map[string]string{
		"positive":               `func contract(t *V2Transition,p runtimeReadyProof,r authorityLedgerRevision,e mh.Edge) { t.publishCheckpointAfterReady(context.Background(),JournalRecord{},p);t.cleanupReady(context.Background(),"tx",p);ledgerMigrationWork(PlannerStepIntent{},MigrationDefinitionV2{},e,r,nil) }`,
		"completion-publication": `func contract(t *V2Transition,p mh.Completion) {t.publishCheckpointAfterReady(context.Background(),JournalRecord{},p)}`,
		"completion-success":     `func contract(t *V2Transition,p mh.Completion) {t.readyResult(p)}`,
		"projection-authority":   `func contract(p compatibilityProjectionRevision,e mh.Edge) {ledgerMigrationWork(PlannerStepIntent{},MigrationDefinitionV2{},e,p,nil)}`,
		"wrong-recipe":           `func contract(p ownerRegistrationV2Plan) { _ = settingsWorkRecipe{file:p} }`,
		"raw-work-phase":         `func contract(t *V2Transition,p v2Work) {t.convergeStep(context.Background(),&JournalRecord{},&ProtectedSnapshot{},p,ReleaseLinks{})}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			source := `package releasetransition
import "context"
import mh "github.com/Subyard/Subyard/internal/migrationhistory"
var _ = context.Background
var _ mh.Edge
` + body
			fixture := filepath.Join(dir, name+".go")
			testkit.WriteFile(t, fixture, []byte(source), 0o600)
			payload, err := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(root, "internal/releasetransition/history_contract_test.go"): fixture}})
			if err != nil {
				t.Fatal(err)
			}
			overlay := filepath.Join(dir, name+".json")
			testkit.WriteFile(t, overlay, payload, 0o600)
			cmd := exec.Command("go", "test", "-overlay", overlay, "-run", "^$", "./internal/releasetransition")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "TMPDIR=/tmp", "GOWORK=off")
			output, err := cmd.CombinedOutput()
			if name == "positive" {
				if err != nil {
					t.Fatalf("positive private consumer: %v\n%s", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(string(output), "cannot use p") {
				t.Fatalf("compiler did not reject forbidden consumer input: %v\n%s", err, output)
			}
		})
	}
}
