package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func TestRollbackAgentCleanupIsVisibleWithThePlanAndResult(t *testing.T) {
	for _, action := range []domain.ActionID{"update.rollback", releaseruntime.PrepareRollbackAction} {
		t.Run(string(action), func(t *testing.T) {
			var output bytes.Buffer
			cli := &CLI{options: Options{Stdout: &output}}
			execution := &releaseExecution{prepared: releaseruntime.Prepared{
				Action:       action,
				AgentCleanup: []releasetransition.RollbackBoundary{{Reason: "approval policy survives rollback", Recovery: "stop agents and restore the previous managed policy", Files: []string{"/etc/sample/48-policy.json", "/etc/sample/49-policy.json", "/etc/sample/51-policy.json"}}},
			}}
			// The same rendering accompanies a displayed operation plan, including --yes.
			cli.printUpdateAgentCleanup(execution)
			plan := output.String()
			output.Reset()
			cli.printUpdateResult(execution, true)
			for _, rendered := range []string{plan, output.String()} {
				for _, want := range []string{"manual cleanup; rollback proceeds", "/etc/sample/48-policy.json", "/etc/sample/49-policy.json", "/etc/sample/51-policy.json", "stop agents and restore the previous managed policy"} {
					if !strings.Contains(rendered, want) {
						t.Fatalf("missing %q from %s", want, rendered)
					}
				}
			}
		})
	}
}

func TestRollbackCommandPrintsCleanupOncePerPlan(t *testing.T) {
	for _, assumeYes := range []bool{false, true} {
		t.Run(fmt.Sprint("yes=", assumeYes), func(t *testing.T) {
			root, environment, runtimeRoot := updateReleaseFixture(t)
			environment = append(environment, "ACTIVE_CAPTURE="+filepath.Join(root, "active.log"))
			prepareCLIReleaseLinks(t, runtimeRoot, environmentValue(environment, "SUBYARD_CONFIG_HOME"), true)
			active := filepath.Join(runtimeRoot, "releases/current-a")
			target := filepath.Join(runtimeRoot, "releases/previous-b")
			engine := filepath.Join(target, "bin/yard-engine")
			data, err := os.ReadFile(engine)
			if err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, engine, strings.Replace(string(data), "yard-engine 1.2.3", "yard-engine 1.2.2", 1), 0o700)
			declaration := releasetransition.RollbackDeclaration{SchemaVersion: 1, PredecessorRegistryDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)), Migrations: []json.RawMessage{}, ProfileFormats: []json.RawMessage{}, Boundaries: []releasetransition.RollbackBoundary{{Component: "agents", ThroughVersion: "1.2.2", Reason: "policies changed or added survive", Recovery: "restore the previous managed policy", SourceDigest: releasetransition.Fingerprint(strings.Repeat("b", 64)), Files: []string{"/etc/sample/policy.json"}}}}
			data, err = json.Marshal(declaration)
			if err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(active, "config/release-rollback.json"), string(data), 0o600)
			for _, release := range []string{active, target} {
				paths := []string{"bin/yard", "bin/yard-engine", "config/release-transition.json"}
				if release == active {
					paths = append(paths, "config/release-rollback.json")
				}
				var manifest strings.Builder
				for _, path := range paths {
					data, err := os.ReadFile(filepath.Join(release, path))
					if err != nil {
						t.Fatal(err)
					}
					fmt.Fprintf(&manifest, "%x  ./%s\n", sha256.Sum256(data), path)
				}
				writeCLIFile(t, filepath.Join(release, "runtime-files.sha256"), manifest.String(), 0o600)
			}
			args := []string{"update", "--rollback", "--runtime-root", runtimeRoot}
			if assumeYes {
				args = append(args, "--yes")
			}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: args, Environment: environment, WorkingDir: root,
				Stdin: strings.NewReader("y\n"), Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			program.promptInputTerminal = func() bool { return true }
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("exit=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
			}
			plan, result, ok := strings.Cut(stdout.String(), "[ ok ] Update completed successfully")
			if !ok {
				t.Fatalf("missing result: %s", stdout.String())
			}
			for _, part := range []string{plan, result} {
				for _, note := range []string{"policies changed or added survive", "Surviving yard file: /etc/sample/policy.json", "Manual cleanup: restore the previous managed policy"} {
					if strings.Count(part, note) != 1 {
						t.Fatalf("cleanup %q not shown exactly once: %s", note, part)
					}
				}
			}
		})
	}
}
