package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

func cleanupMode(arguments []string) (bool, error) {
	soft, hard := false, false
	for _, argument := range arguments {
		switch argument {
		case "--yes", "-y":
		case "--soft":
			soft = true
		case "--hard":
			hard = true
		default:
			return false, fmt.Errorf("unknown cleanup option %q", argument)
		}
	}
	if soft && hard {
		return false, errors.New("choose either --soft or --hard")
	}
	return hard, nil
}

func pendingHardCleanup(yard domain.Context) error {
	if _, err := os.Lstat(hardCleanupPath(yard)); err == nil {
		return errors.New("project-preserving hard cleanup is pending for this yard; resume cleanup --hard")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (cli *CLI) cleanupPaths(loaded config.Loaded) ([]string, error) {
	var paths []string
	for _, name := range strings.Fields(loaded.Environment["ENVIRONMENT_PROFILES"]) {
		pkg, err := profile.ResolvePackage(cli.options.RepositoryRoot, name)
		if err != nil {
			return nil, err
		}
		values, err := config.ProfileValues(pkg.Config, loaded.Environment)
		if err != nil {
			return nil, err
		}
		caches, err := profile.CleanupCaches(values["CLEANUP_CACHES"], loaded.Context.DevUser)
		if err != nil {
			return nil, fmt.Errorf("profile %s cache declaration: %w", name, err)
		}
		paths = append(paths, caches...)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	// Nested declarations are covered by their explicit ancestor once. The root
	// remains preserved; no arbitrary parent is inferred from several children.
	var merged []string
	for _, path := range paths {
		covered := false
		for _, parent := range merged {
			covered = covered || strings.HasPrefix(path, parent+"/")
		}
		if !covered {
			merged = append(merged, path)
		}
	}
	return merged, nil
}

func (prepared *preparedCommand) prepareCleanup(ctx context.Context, _ *initBootstrap) error {
	hard, err := cleanupMode(prepared.Arguments)
	if err != nil {
		return err
	}
	if hard {
		return prepared.prepareHardCleanup(ctx)
	}
	cli, loaded := prepared.CLI, prepared.Loaded
	if err := pendingHardCleanup(loaded.Context); err != nil {
		return err
	}
	paths, err := cli.cleanupPaths(loaded)
	if err != nil {
		return err
	}
	payload, err := os.ReadFile(filepath.Join(cli.options.RepositoryRoot, "scripts/lib/yard-cleanup.py"))
	if err != nil {
		return err
	}
	data := cli.projectDataPlane()
	assess := func(ctx context.Context) ([]byte, error) {
		request := ports.InstanceExecRequest{Command: append([]string{"python3", "-c", string(payload), "assess"}, paths...)}
		result, err := data.Execute(ctx, loaded.Context, request)
		if err != nil || result.ExitCode != 0 {
			return nil, fmt.Errorf("assess cleanup directories: %w", errors.Join(err, errors.New(string(result.Stderr))))
		}
		// Filesystem identities are full-width integers. Keep their JSON tokens
		// exact while checking the root-object array returned by the helper.
		var roots []map[string]json.RawMessage
		if err := json.Unmarshal(result.Stdout, &roots); err != nil || len(roots) != len(paths) {
			return nil, errors.New("cleanup directory assessment is incomplete")
		}
		for _, root := range roots {
			if root == nil {
				return nil, errors.New("cleanup directory assessment is incomplete")
			}
		}
		return json.Marshal(roots)
	}
	roots, err := assess(ctx)
	if err != nil {
		return err
	}
	consequences := []string{"remove only explicitly declared disposable cache contents; an active build may need a retry"}
	for _, path := range paths {
		consequences = append(consequences, "clear "+path)
	}
	prepared.policy.Consequences = consequences
	prepared.exactState = operationStateDigest([]any{loaded.Environment, paths, string(payload), string(roots)})
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return "yard.cleanup.soft", domain.ActionDelta{Changed: len(paths) != 0, Consequences: consequences}, nil
	}
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		current, err := assess(ctx)
		if err != nil {
			return "", domain.ActionDelta{}, err
		}
		if string(current) != string(roots) {
			return "", domain.ActionDelta{}, domain.ErrPlanStale
		}
		return prepared.assess(ctx)
	}
	prepared.stepsComplete = true
	prepared.steps = func() []domain.OperationStep {
		var steps []domain.OperationStep
		for index, path := range paths {
			desired := "remove disposable contents while preserving directory metadata and nested mounts"
			steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("cleanup.directory.%d", index), Target: loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName + ":" + path, Observed: "captured directory identity or absent", Desired: desired, Verify: "native no-follow descriptor traversal preserves approved root identity", Decision: domain.StepApply, Consequence: "an active build may need a retry"})
		}
		return steps
	}
	prepared.execute = func(ctx context.Context, _ *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		if _, _, err := prepared.refresh(ctx); err != nil {
			return domain.AdapterResult{}, err
		}
		result, err := data.Execute(ctx, loaded.Context, ports.InstanceExecRequest{Command: []string{"python3", "-c", string(payload), "apply"}, Stdin: roots})
		if err != nil || result.ExitCode != 0 {
			return domain.AdapterResult{}, fmt.Errorf("clear declared yard directories: %w", errors.Join(err, errors.New(string(result.Stderr))))
		}
		var report struct {
			Entries     int `json:"entries_removed"`
			Mounts      int `json:"mounts_preserved"`
			Filesystems []struct {
				Path   string `json:"path"`
				Change int64  `json:"change"`
			} `json:"filesystems"`
		}
		if err := json.Unmarshal(result.Stdout, &report); err != nil {
			return domain.AdapterResult{}, errors.New("cleanup completed but its space report is unavailable")
		}
		fmt.Fprintf(diagnostics, "Removed %d disposable entries; preserved %d nested mounts.\n", report.Entries, report.Mounts)
		for _, filesystem := range report.Filesystems {
			fmt.Fprintf(diagnostics, "Filesystem at %s: available-space change %+d bytes (concurrent changes included).\n", filesystem.Path, filesystem.Change)
		}
		fmt.Fprintln(diagnostics, "Space may remain held by hardlinks, open files or shared storage.")
		return domain.AdapterResult{Schema: 1, OperationID: prepared.Plan.OperationID, Status: "ok"}, nil
	}
	return nil
}
