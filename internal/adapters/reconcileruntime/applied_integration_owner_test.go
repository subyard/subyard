package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// Run the production inventory reader against protected /tmp metadata. Selection
// reads validate home-bound strings without touching the named guest home.
type appliedOwnerInventoryExecutor struct {
	state           string
	requests        []ports.InstanceExecRequest
	beforeSelection func()
}

func (executor *appliedOwnerInventoryExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	executor.requests = append(executor.requests, request)
	if request.Command[4] == "selection" && executor.beforeSelection != nil {
		executor.beforeSelection()
	}
	program := strings.ReplaceAll(request.Command[3], "/var/lib/subyard/integrations", executor.state)
	program = strings.ReplaceAll(program, "OWNER = int('0')", fmt.Sprintf("OWNER = int('%d')", os.Getuid()))
	command := exec.CommandContext(ctx, "python3", "-B", "-c", program, request.Command[4])
	command.Stdin = strings.NewReader(string(request.Stdin))
	output, err := command.CombinedOutput()
	result := ports.InstanceExecResult{Stdout: output}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
		}
	}
	return result, err
}

func TestAppliedIntegrationOwnerReadPrecedesHomeValidation(t *testing.T) {
	for _, scenario := range []string{"applied-owner", "foreign-record", "foreign-released", "unsafe-mode", "invalid-owner", "changed-baseline"} {
		t.Run(scenario, func(t *testing.T) {
			root := testkit.TempDir(t)
			state := filepath.Join(root, "inventory")
			if err := os.Mkdir(state, 0o700); err != nil {
				t.Fatal(err)
			}
			settings := map[string]string{"DEV_USER": "dev", "DEV_UID": "1001"}
			recordPath, releasedPath := "/home/dev/.sample/config", "/home/dev/.sample/retired"
			if scenario == "foreign-record" {
				recordPath = "/home/other/.sample/config"
			}
			if scenario == "foreign-released" {
				releasedPath = "/home/other/.sample/retired"
			}
			if scenario == "invalid-owner" {
				settings["DEV_USER"] = "../other"
			}
			inventory := map[string]any{"schema": 1, "entries": []integrationArtifact{{ID: "sample", Kind: "file", Path: recordPath, Digest: strings.Repeat("a", 64)}, {ID: "sample", Kind: "package", Digest: strings.Repeat("b", 64)}}, "released": []string{releasedPath}, "applied_settings": settings}
			path := filepath.Join(state, "inventory.json")
			write := func(mode os.FileMode) {
				payload, _ := json.Marshal(inventory)
				testkit.WriteFile(t, path, payload, mode)
			}
			write(0o600)
			if scenario == "unsafe-mode" {
				write(0o644)
			}
			executor := &appliedOwnerInventoryExecutor{state: state}
			if scenario == "changed-baseline" {
				executor.beforeSelection = func() { settings["DEV_USER"] = "other"; write(0o600) }
			}
			runtime := Runtime{RepositoryRoot: root, Executor: executor, Integrations: []profile.IntegrationDefinition{}, Yard: domain.Context{DevUser: "other", DevUID: 1002}}
			applied, err := runtime.AppliedIntegrationConfiguration(context.Background())
			if scenario != "applied-owner" {
				if err == nil {
					t.Fatal("invalid or changed applied ownership was accepted")
				}
				if scenario == "changed-baseline" && !errors.Is(err, domain.ErrPlanStale) {
					t.Fatalf("changed baseline: %v", err)
				}
				return
			}
			if err != nil || !applied.Installed || applied.Settings["DEV_USER"] != "dev" || strings.Join(applied.Selected, " ") != "sample" || len(executor.requests) != 2 {
				t.Fatalf("applied owner read: %#v %v calls=%d", applied, err, len(executor.requests))
			}
			var request struct {
				Home string
				UID  int
			}
			if err := json.Unmarshal(executor.requests[1].Stdin, &request); err != nil {
				t.Fatal(err)
			}
			if request.Home != "/home/dev" || request.UID != 1001 || runtime.Yard.DevUser != "other" {
				t.Fatalf("home validation did not use the applied owner: %#v", request)
			}
		})
	}
}
