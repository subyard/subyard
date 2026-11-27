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
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Execute the real inventory in a disposable guest filesystem; all other native
// phases retain the existing materialized-owner fixture contracts.
type inventoryExecutor struct {
	*retryableIntegrationExecutor
	root, home, state string
	mutations         int
}

func (e *inventoryExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
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
		// Refusals name guest paths, as the real guest reports them.
		result := ports.InstanceExecResult{Stdout: stdout.Bytes(), Stderr: bytes.ReplaceAll(stderr.Bytes(), []byte(e.home), []byte("/home/dev"))}
		if err != nil {
			result.ExitCode = 1
			return result, &ports.GuestExitError{Code: 1}
		}
		if request.Command[4] != "observe" && err == nil {
			e.mutations++
		}
		return result, nil
	}
	if len(request.Command) > 5 && request.Command[3] == configmaterial.CompareWriteProgram() {
		// Run the real guest compare-and-write against this fixture's home.
		arguments := slices.Clone(request.Command[4:])
		for index, argument := range arguments {
			if argument == "/home/dev" || strings.HasPrefix(argument, "/home/dev/") {
				arguments[index] = e.home + strings.TrimPrefix(argument, "/home/dev")
			}
		}
		if arguments[0] == "write" {
			arguments[3] = fmt.Sprint(os.Getuid())
		}
		command := exec.CommandContext(ctx, "python3", append([]string{"-B", "-c", request.Command[3]}, arguments...)...)
		command.Stdin = bytes.NewReader(request.Stdin)
		var stdout bytes.Buffer
		command.Stdout = &stdout
		err := command.Run()
		result := ports.InstanceExecResult{Stdout: bytes.ReplaceAll(stdout.Bytes(), []byte(e.home), []byte("/home/dev")), ExitCode: command.ProcessState.ExitCode()}
		if err != nil {
			return result, &ports.GuestExitError{Code: result.ExitCode}
		}
		if arguments[0] == "write" {
			e.mutations++
		}
		return result, nil
	}
	return e.retryableIntegrationExecutor.Exec(ctx, project, instance, request)
}

// legacyPreviewBlock is a managed block that older releases appended to copies.
const legacyPreviewBlock = "<!-- subyard-preview -->\nFor a static web preview, run `subyard-preview <relative-static-dir>` from the Git workspace and keep it running with your background/async process mechanism. Share the printed URL. An owner Tailscale URL requires device reachability and Tailnet policy access; a loopback URL requires an active preview-enabled `yard code` SSH session. The helper must stay running for either URL.\n<!-- /subyard-preview -->\n"

// retiredInstructionFixture owns a Claude instruction copy that an older release
// recorded in the inventory; current packages no longer declare it.
func retiredInstructionFixture(t *testing.T, selected bool, body []byte) (Runtime, *inventoryExecutor, string) {
	t.Helper()
	root := testkit.TempDir(t)
	runtime, base := materializedIntegrationFixture(t, root, true)
	// Keep the existing sample lifecycle fixture while adding a Claude package.
	claude := Runtime{Environment: runtime.Environment}
	bindAgentTestPackage(t, &claude, root, "claude")
	for _, definition := range claude.Integrations {
		if definition.Name == "claude" {
			runtime.Integrations = append(runtime.Integrations, definition)
		}
	}
	executor := &inventoryExecutor{retryableIntegrationExecutor: base, root: root, home: root + "/guest-home", state: root + "/inventory"}
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
		runtime.Environment = append(runtime.Environment, "CODING_TOOL_INTEGRATIONS=sample claude")
	}
	return runtime, executor, path
}

// An unchanged instruction copy that Subyard owns is removed at the next apply,
// with or without an older managed block, and the result converges.
func TestRetiredInstructionCopyIsRemovedWhenUnchanged(t *testing.T) {
	for name, body := range map[string]string{"plain": "Copied host text.\n", "preview": "Copied host text.\n\n" + legacyPreviewBlock} {
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprint(name, "/selected=", selected), func(t *testing.T) {
				runtime, executor, path := retiredInstructionFixture(t, selected, []byte(body))
				plan, err := runtime.IntegrationPlan(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if err := runtime.ApplyIntegrations(context.Background(), plan); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("retired instruction copy kept: %v", err)
				}
				data, err := os.ReadFile(executor.state + "/inventory.json")
				if err != nil || bytes.Contains(data, []byte(path)) {
					t.Fatalf("retired instruction copy is still recorded: %s %v", data, err)
				}
				verified, err := runtime.IntegrationPlan(context.Background())
				if err != nil || verified.Changed {
					t.Fatalf("retirement did not converge: changed=%v err=%v", verified.Changed, err)
				}
			})
		}
	}
}

// A copy edited in the yard is not Subyard's to delete: the plan refuses it by
// path and nothing changes.
func TestRetiredInstructionCopyEditedInTheYardIsKept(t *testing.T) {
	runtime, executor, path := retiredInstructionFixture(t, true, []byte("Copied host text.\n"))
	edited := []byte("Copied host text.\nEdited in the yard.\n")
	testkit.WriteFile(t, path, edited, 0o644)
	before, _ := os.ReadFile(executor.state + "/inventory.json")
	_, err := runtime.IntegrationPlan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "owned artifact drift") || !strings.Contains(err.Error(), `"/home/dev/.claude/CLAUDE.md"`) {
		t.Fatalf("edited copy was not refused by path: %v", err)
	}
	after, _ := os.ReadFile(executor.state + "/inventory.json")
	actual, _ := os.ReadFile(path)
	if executor.mutations != 0 || !bytes.Equal(before, after) || !bytes.Equal(actual, edited) {
		t.Fatal("refused retirement changed files")
	}
}
