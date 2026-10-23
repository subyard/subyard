package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Execute the real inventory in a disposable guest filesystem; all other native
// phases retain the existing materialized-owner fixture contracts.
type previewInventoryExecutor struct {
	*retryableIntegrationExecutor
	root, home, state string
	mutations         int
}

func (e *previewInventoryExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if len(request.Command) >= 5 && request.Command[0] == "python3" && strings.Contains(request.Command[3], "Version 1 records observed ownership only") {
		replacements := strings.NewReplacer("/var/lib/subyard/integrations", e.state, "OWNER = int('0')", fmt.Sprintf("OWNER = int('%d')", os.Getuid()), "/usr/local/libexec/subyard/projects-changed", e.root+"/core/projects-changed", "/etc/subyard/agent-project-hooks", e.root+"/core/agent-project-hooks")
		program := replacements.Replace(request.Command[3])
		input := strings.NewReplacer("/home/dev", e.home, "/usr/local/libexec/subyard/projects-changed", e.root+"/core/projects-changed", "/etc/subyard/agent-project-hooks", e.root+"/core/agent-project-hooks").Replace(string(request.Stdin))
		command := exec.CommandContext(ctx, "python3", "-B", "-c", program, request.Command[4])
		command.Stdin = strings.NewReader(input)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		err := command.Run()
		result := ports.InstanceExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
		if err != nil {
			result.ExitCode = 1
		}
		if request.Command[4] != "observe" && err == nil {
			e.mutations++
		}
		return result, nil
	}
	if len(request.Command) > 5 && request.Command[0] == "sh" && strings.HasPrefix(request.Command[5], "/home/dev/") {
		path := strings.Replace(request.Command[5], "/home/dev", e.home, 1)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return ports.InstanceExecResult{}, err
		}
		if err := os.WriteFile(path, request.Stdin, 0o644); err != nil {
			return ports.InstanceExecResult{}, err
		}
		if err := os.Chmod(path, 0o644); err != nil {
			return ports.InstanceExecResult{}, err
		}
		if err := os.Chown(path, -1, os.Getuid()); err != nil {
			return ports.InstanceExecResult{}, err
		}
		e.mutations++
		return ports.InstanceExecResult{}, nil
	}
	return e.retryableIntegrationExecutor.Exec(ctx, project, instance, request)
}

func preparedPreviewFixture(t *testing.T, selected bool, body []byte) (Runtime, *previewInventoryExecutor, string) {
	t.Helper()
	root := testkit.TempDir(t)
	runtime, base := materializedIntegrationFixture(t, root, true)
	// Current instruction selection comes from a real package declaration.
	// Keep the existing sample lifecycle fixture while adding the instruction owner.
	instructions := Runtime{Environment: runtime.Environment}
	bindInstructionTestPackage(t, &instructions, root, "claude")
	for _, definition := range instructions.Integrations {
		if definition.Name == "claude" {
			runtime.Integrations = append(runtime.Integrations, definition)
		}
	}
	executor := &previewInventoryExecutor{retryableIntegrationExecutor: base, root: root, home: root + "/guest-home", state: root + "/inventory"}
	runtime.Executor = executor
	path := executor.home + "/.claude/CLAUDE.md"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, body, 0o644)
	if err := os.Chown(path, -1, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(executor.state, 0o700); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"schema": 1, "entries": []integrationArtifact{{ID: "claude", Kind: "file", Path: path, Digest: fmt.Sprintf("%x", sha256.Sum256(body))}}})
	testkit.WriteFile(t, executor.state+"/inventory.json", payload, 0o600)
	if selected {
		runtime.Environment = append(runtime.Environment, "CODING_TOOL_INTEGRATIONS=sample claude", "HOST_CLAUDE_MD="+root+"/missing-host-source")
	}
	return runtime, executor, path
}

func TestCurrentPreparedPreviewRetirementMigratesBeforeStaticRefresh(t *testing.T) {
	original := []byte("Retained host text.\n\n" + previewInstructions)
	want := []byte("Retained host text.\n\n\n")
	for _, selected := range []bool{false, true} {
		t.Run(fmt.Sprint("selected=", selected), func(t *testing.T) {
			runtime, executor, path := preparedPreviewFixture(t, selected, original)
			prepared := prepareMaterializedIntegrationFixture(t, runtime)
			defer prepared.Close()
			if selected && len(prepared.configs.files) != 1 {
				t.Fatal("missing-source case did not select the instruction adapter")
			}
			rebound, err := runtime.BindInstalledRuntimeContext(context.Background(), prepared.Recipe())
			if err != nil {
				t.Fatal(err)
			}
			defer rebound.Close()
			if err := runtime.ApplyPreparedMaterializedIntegrations(context.Background(), rebound); err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(actual, want) {
				t.Fatalf("unmarked text lost before core refresh: %q %v", actual, err)
			}
			data, err := os.ReadFile(executor.state + "/inventory.json")
			if err != nil || !bytes.Contains(data, []byte(path)) {
				t.Fatal("retained instruction ownership was not released", err)
			}
			verified, err := runtime.IntegrationPlan(context.Background())
			if err != nil || verified.Changed {
				t.Fatalf("retained text prevented repeat convergence: changed=%v err=%v", verified.Changed, err)
			}
			// This test never invokes static skill refresh or its migration script.
		})
	}
}

func TestHistoricalPreparedEmptyInstructionSelectionKeepsRetirementSemantics(t *testing.T) {
	runtime, _, path := preparedPreviewFixture(t, false, []byte("Historical text.\n\n"+previewInstructions))
	runtime.legacyPreviewInstructions = true
	prepared := prepareMaterializedIntegrationFixture(t, runtime)
	defer prepared.Close()
	raw := prepared.Recipe()
	metadata, _, err := readMaterializedMetadata(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(metadata, []byte("PreviewSkillCutover")) {
		t.Fatal("historical canonical bytes gained current intent")
	}
	current := runtime
	current.legacyPreviewInstructions = false
	rebound, err := current.BindInstalledRuntimeContext(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer rebound.Close()
	if !bytes.Equal(rebound.Recipe(), raw) {
		t.Fatal("historical empty-selector recipe bytes changed")
	}
	if err := current.ApplyPreparedMaterializedIntegrations(context.Background(), rebound); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("historical empty-selector retirement changed: %v", err)
	}
}

func TestCurrentPreparedPreviewRefusesUnknownMarkersBeforeAnyMutation(t *testing.T) {
	unknown := []byte("Guest text.\n<!-- subyard-preview -->\nUser-owned unknown body.\n<!-- /subyard-preview -->\n")
	runtime, executor, path := preparedPreviewFixture(t, true, unknown)
	host := filepath.Join(runtime.RepositoryRoot, "new-host-source")
	testkit.WriteFile(t, host, []byte("Current host instructions.\n"), 0o600)
	runtime.Environment = preparedInstalledEnvironment(runtime.Environment, "HOST_CLAUDE_MD", host)
	// Even a valid ownership digest cannot authorize replacing an unknown block.
	before, _ := os.ReadFile(executor.state + "/inventory.json")
	if _, err := runtime.IntegrationPlan(context.Background()); err == nil {
		t.Fatal("unknown block admitted to current materialized plan")
	}
	after, _ := os.ReadFile(executor.state + "/inventory.json")
	actual, _ := os.ReadFile(path)
	if executor.mutations != 0 || !bytes.Equal(before, after) || !bytes.Equal(actual, unknown) {
		t.Fatal("refused current migration changed files")
	}
}

func TestCurrentPreparedConfigOnlyMigratesWithMissingHostSource(t *testing.T) {
	runtime, _, path := preparedPreviewFixture(t, true, []byte("Retained config-only text.\n"+previewInstructions))
	configs, err := runtime.PrepareConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := runtime.PrepareMaterializedConfigs(context.Background(), configs)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	rebound, err := runtime.BindInstalledRuntimeContext(context.Background(), prepared.Recipe())
	if err != nil {
		t.Fatal(err)
	}
	defer rebound.Close()
	if err := runtime.ApplyPreparedMaterializedConfigs(context.Background(), rebound); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != "Retained config-only text.\n\n" {
		t.Fatalf("config-only current migration: %q %v", actual, err)
	}
}

func TestCurrentPreparedSelectedHostSourceMigratesBeforeConfigWrite(t *testing.T) {
	runtime, _, path := preparedPreviewFixture(t, true, []byte("Prior host text.\n"+previewInstructions))
	source := filepath.Join(runtime.RepositoryRoot, "current-host-source")
	want := []byte("Current source-owned instructions.\n")
	testkit.WriteFile(t, source, want, 0o600)
	runtime.Environment = preparedInstalledEnvironment(runtime.Environment, "HOST_CLAUDE_MD", source)
	prepared := prepareMaterializedIntegrationFixture(t, runtime)
	defer prepared.Close()
	if err := runtime.ApplyPreparedMaterializedIntegrations(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, want) {
		t.Fatalf("selected source migration: %q %v", actual, err)
	}
	original, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(original, want) {
		t.Fatal("host source changed")
	}
}
