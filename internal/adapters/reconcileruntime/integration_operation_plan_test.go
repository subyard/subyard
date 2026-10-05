package reconcileruntime

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestNativeInventoryConvergenceCannotExpandApprovedIntegrationWork(t *testing.T) {
	root := testkit.TempDir(t)
	home, state := filepath.Join(root, "home"), filepath.Join(root, "inventory")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	program := strings.ReplaceAll(integrationInventoryProgram, "@STATE_ROOT@", state)
	program = strings.ReplaceAll(program, "@STATE_UID@", fmt.Sprint(os.Getuid()))
	entries := []integrationArtifact{}
	for _, name := range []string{"first", "second"} {
		entries = append(entries, integrationArtifact{ID: name, Kind: "file", Path: filepath.Join(home, name), Content: base64.StdEncoding.EncodeToString([]byte(name)), Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))})
	}
	run := func(mode string, selected []integrationArtifact) integrationObservation {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"home": home, "uid": os.Getuid(), "entries": selected})
		command := exec.Command("python3", "-B", "-c", program, mode)
		command.Stdin = strings.NewReader(string(payload))
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("native %s: %v %s", mode, err, output)
		}
		var result integrationObservation
		if mode == "observe" {
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	plan := func(observation integrationObservation) IntegrationPlan {
		return IntegrationPlan{Fingerprint: observation.Fingerprint, Changed: observation.Changed, Scope: "fixed two-artifact scope", facts: observation.Facts}
	}
	approved := plan(run("observe", entries))
	run("apply", entries[:1])
	run("commit", entries[:1])
	partial := plan(run("observe", entries))
	if err := CheckIntegrationPlan(approved, partial); err != nil {
		t.Fatalf("native partial convergence denied: %v", err)
	}
	if !partial.Changed || !partial.facts[0].AtDesired || partial.facts[1].AtDesired {
		t.Fatal("native independent targets were not observed separately")
	}
	// This second plan approved the first target as converged. Its disappearance
	// cannot add a fresh installation to the remaining approved work.
	if err := os.Remove(entries[0].Path); err != nil {
		t.Fatal(err)
	}
	if err := CheckIntegrationPlan(partial, plan(run("observe", entries))); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("original skip became new work: %v", err)
	}
}
