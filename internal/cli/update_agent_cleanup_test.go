package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	for _, resumed := range []bool{false, true} {
		for _, assumeYes := range []bool{false, true} {
			t.Run(fmt.Sprint("resumed=", resumed, "/yes=", assumeYes), func(t *testing.T) {
				fixture, root, environment, runtimeRoot := newRollbackBoundaryFixture(t, func(engine string) string {
					if resumed {
						engine = strings.ReplaceAll(engine, `"status":"migration-required"`, `"status":"recovering","transaction":"tx-0123456789abcdef"`)
						engine = strings.ReplaceAll(engine, `"code":"transition-required"`, `"code":"recovery-pending"`)
						engine = strings.ReplaceAll(engine, `"inspection":{"plan":`, `"inspection":{"resume":"tx-0123456789abcdef","plan":`)
						engine = strings.ReplaceAll(engine, `"resume":"tx-0123456789abcdef","plan":"plan-v1-`, `"resume":"tx-0123456789abcdef","plan":"resume-v1-`)
					}
					return engine
				})
				args := []string{"update", "--rollback", "--runtime-root", runtimeRoot}
				if assumeYes {
					args = append(args, "--yes")
				}
				stdout := cleanupOrderWriter{runtimeRoot: runtimeRoot}
				var stderr bytes.Buffer
				program := fixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: args, Environment: environment, WorkingDir: root,
					Stdin: strings.NewReader("y\n"), Stdout: &stdout, Stderr: &stderr})
				program.promptInputTerminal = func() bool { return true }
				if code := program.Run(context.Background()); code != 0 {
					t.Fatalf("exit=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
				}
				if !slices.Equal(stdout.links, []string{"releases/current-a", "releases/previous-b"}) {
					t.Fatalf("cleanup was not shown before execution and in result: %v", stdout.links)
				}
				plan, result, ok := strings.Cut(stdout.String(), "[ ok ] Update completed successfully")
				if !ok {
					t.Fatalf("missing result: %s", stdout.String())
				}
				if resumed && strings.Contains(plan, "Proceed?") {
					t.Fatalf("resume unexpectedly prompted: %s", plan)
				}
				for _, part := range []string{plan, result} {
					for _, note := range []string{"policies changed or added survive", "Affected yard path: /etc/sample/policy.json", "Manual cleanup: restore the previous managed policy"} {
						if strings.Count(part, note) != 1 {
							t.Fatalf("cleanup %q not shown exactly once: %s", note, part)
						}
					}
				}
			})
		}
	}
}

// newRollbackBoundaryFixture prepares a rollback to 1.2.2 whose active release
// declares one agent boundary; patch edits the retained engine script.
func newRollbackBoundaryFixture(t *testing.T, patch func(string) string) (*cliFixture, string, []string, string) {
	t.Helper()
	fixture, root, environment, runtimeRoot := newUpdateReleaseFixture(t)
	environment = append(environment, "ACTIVE_CAPTURE="+filepath.Join(root, "active.log"))
	prepareCLIReleaseLinks(t, runtimeRoot, environmentValue(environment, "SUBYARD_CONFIG_HOME"), true)
	active := filepath.Join(runtimeRoot, "releases/current-a")
	target := filepath.Join(runtimeRoot, "releases/previous-b")
	engine := filepath.Join(target, "bin/yard-engine")
	data, err := os.ReadFile(engine)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, engine, patch(strings.Replace(string(data), "yard-engine 1.2.3", "yard-engine 1.2.2", 1)), 0o700)
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
	return fixture, root, environment, runtimeRoot
}

// A retained release that refuses to plan, as one refusing a file at a declared
// agent path does, gets the boundary's paths and recovery as its next step.
func TestRollbackInspectionFailureNamesAgentBoundaryRecovery(t *testing.T) {
	fixture, root, environment, runtimeRoot := newRollbackBoundaryFixture(t, func(engine string) string {
		return strings.Replace(engine, `if [ "${UPDATE_CANCEL_INSPECTION:-}" = 1 ]; then exec sleep 30; fi`, `exit 3`, 1)
	})
	var stdout, stderr bytes.Buffer
	program := fixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"update", "--rollback", "--runtime-root", runtimeRoot, "--yes"},
		Environment: environment, WorkingDir: root, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
	if code := program.Run(context.Background()); code == 0 {
		t.Fatalf("rollback succeeded: %s", stdout.String())
	}
	rendered := stdout.String() + stderr.String()
	for _, want := range []string{
		"the retained release cannot provide a safe rollback plan; declared rollback limitations: policies changed or added survive Paths: /etc/sample/policy.json",
		"follow the declared recovery, then retry yard update --rollback: restore the previous managed policy",
	} {
		if strings.Count(rendered, want) != 1 {
			t.Fatalf("refusal does not name %q exactly once: %s", want, rendered)
		}
	}
	if strings.Contains(rendered, "restore a compatible retained release") {
		t.Fatalf("refusal kept the generic next step: %s", rendered)
	}
}

// Capture runtime state when notices are written, proving plan output precedes
// the real command's activation rather than merely preceding the result heading.
type cleanupOrderWriter struct {
	bytes.Buffer
	runtimeRoot string
	links       []string
}

func (writer *cleanupOrderWriter) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "Manual cleanup:") {
		link, err := os.Readlink(filepath.Join(writer.runtimeRoot, "current"))
		if err != nil {
			link = err.Error()
		}
		writer.links = append(writer.links, link)
	}
	return writer.Buffer.Write(data)
}

func (writer *cleanupOrderWriter) WriteString(data string) (int, error) {
	return writer.Write([]byte(data))
}

func TestRollbackCleanupReadWarningIsVisibleWithPlanAndResult(t *testing.T) {
	const warning = "Warning: cleanup notices could not be read for release 0.31.0-source; recovery proceeds."
	var output bytes.Buffer
	cli := &CLI{options: Options{Stdout: &output}}
	execution := &releaseExecution{prepared: releaseruntime.Prepared{Action: "update.rollback", AgentCleanupWarning: warning}}
	cli.printUpdateAgentCleanup(execution)
	if output.String() != warning+"\n" {
		t.Fatalf("plan warning missing or repeated: %s", output.String())
	}
	output.Reset()
	cli.printUpdateResult(execution, true)
	if strings.Count(output.String(), warning) != 1 {
		t.Fatalf("result warning missing or repeated: %s", output.String())
	}
}
