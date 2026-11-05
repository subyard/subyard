package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

type pendingDiskTmp struct{ Power, UUID string }

// Pending state is read from the native marker, never from a saved exemption.
func (cli *CLI) pendingDiskTmpTargets(ctx context.Context, targets []configTarget) (map[string]pendingDiskTmp, error) {
	found := map[string]pendingDiskTmp{}
	var refusals []error
	native, _ := cli.statusPorts()
	for _, target := range targets {
		yard := target.Loaded.Context
		instance, err := native.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
		if errors.Is(err, ports.ErrInstanceNotFound) {
			continue
		}
		if err != nil {
			refusals = append(refusals, err)
			continue
		}
		if instance.Config["user.subyard.disk_tmp"] == "v1" {
			continue
		}
		if instance.Config["user.subyard.disk_tmp"] != "" {
			refusals = append(refusals, fmt.Errorf("yard %s has foreign guest temporary policy metadata", target.Name))
			continue
		}
		if instance.Config["user.subyard.managed"] == "true" && instance.Config["user.subyard.name"] == target.Name {
			found[target.Name] = pendingDiskTmp{instance.Status, instance.Config["volatile.uuid"]}
		}
	}
	return found, domain.CollectRefusals(refusals...)
}

func diskTmpWarnings(found map[string]pendingDiskTmp, targets []configTarget) []string {
	warnings := []string{}
	for _, target := range targets {
		if pending, ok := found[target.Name]; ok {
			warnings = append(warnings, fmt.Sprintf("disk-/tmp pending (%s): %s; run %s", strings.ToLower(pending.Power), migration.DiskTmpDisclosure(target.Name, strings.EqualFold(pending.Power, "running")), migration.DiskTmpRetry(target.Name, pending.Power)))
		}
	}
	slices.Sort(warnings)
	if len(warnings) > 32 {
		warnings = append(warnings[:32], fmt.Sprintf("disk-/tmp pending: %d additional yards require migration", len(warnings)-32))
	}
	return warnings
}

func (cli *CLI) deferredDiskTmpTargets(ctx context.Context, targets []configTarget, enabled bool) (map[string]pendingDiskTmp, error) {
	if !enabled {
		return nil, nil
	}
	return cli.pendingDiskTmpTargets(ctx, targets)
}

// A current-release retry is an ordinary prepared update. Nothing runs during
// inspection or before its single confirmation; its own steps bind the yard set.
func (cli *CLI) prepareDiskTmpRetry(ctx context.Context, prepared *preparedCommand) error {
	release := prepared.release
	if cli.releaseRepairTarget != "" || release == nil || release.prepared.Action != "update.activate" || release.prepared.SourceRelease == "" || release.prepared.SourceRelease != release.prepared.TargetRelease {
		return nil
	}
	// An unfinished journal belongs entirely to its sealed owner. It may have
	// its own restart consent; never append foreground retry work to a resume.
	store, err := releasetransition.NewPOSIXV2Store(release.configHome)
	if err != nil {
		return err
	}
	journalSnapshot, err := store.ReadCurrentJournal()
	if err != nil || !journalSnapshot.Exists {
		return err
	}
	journal, err := releasetransition.ParseJournal(journalSnapshot.Payload)
	if err != nil {
		return err
	}
	if journal.Checkpoint != releasetransition.JournalComplete || string(journal.Goal.Target) != release.prepared.TargetRelease {
		return nil
	}
	targets, err := cli.releaseConfigTargets(ctx, prepared.Loaded, true)
	if err != nil {
		return err
	}
	captured, err := cli.pendingDiskTmpTargets(ctx, targets)
	if err != nil {
		return err
	}
	owners := []*migration.RegisteredYardMigration{}
	steps := []domain.OperationStep{}
	changed := false
	for _, target := range targets {
		value, pending := captured[target.Name]
		if !pending {
			continue
		}
		owner := cli.registeredYardMigration(releasetransition.ProcessRequest{ConfigHome: prepared.Loaded.Context.Paths.ConfigHome}, true, target.Name).(*migration.RegisteredYardMigration)
		_, err := owner.Observe(ctx, releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
		if err != nil {
			return err
		}
		running := strings.EqualFold(value.Power, "running")
		changed = changed || running
		owners = append(owners, owner)
		decision := domain.StepSkip
		if running {
			decision = domain.StepApply
		}
		steps = append(steps, domain.OperationStep{ID: fmt.Sprintf("release.disk-tmp.%d", len(steps)), Target: "yard " + target.Name, Observed: "disk-/tmp marker absent", Desired: migration.DiskTmpDisclosure(target.Name, running), Decision: decision, Verify: "observe the native disk-/tmp marker; preserve pending failures as warnings", Consequence: migration.DiskTmpDisclosure(target.Name, running)})
	}
	if len(owners) == 0 {
		return nil
	}
	oldSteps, oldAssess, execute := prepared.steps, prepared.assess, prepared.execute
	prepared.exactState = operationStateDigest(struct {
		State   string
		Pending map[string]pendingDiskTmp
	}{prepared.exactState, captured})
	prepared.steps = func() []domain.OperationStep { return append(oldSteps(), domain.CloneOperationSteps(steps)...) }
	prepared.assess = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		action, delta, err := oldAssess(ctx)
		delta.Changed = delta.Changed || changed
		for _, step := range steps {
			delta.Consequences = append(delta.Consequences, step.Consequence)
		}
		return action, delta, err
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, output io.Writer) (domain.AdapterResult, error) {
		fresh, err := cli.pendingDiskTmpTargets(ctx, targets)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if !maps.Equal(fresh, captured) {
			return domain.AdapterResult{}, domain.ErrPlanStale
		}
		return execute(ctx, orchestrator, output)
	}
	release.retryDiskTmp = func(ctx context.Context) error {
		for _, owner := range owners {
			if err := owner.Reconcile(ctx, releasetransition.ReleaseLinks{}); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func (cli *CLI) finishDiskTmpStart(ctx context.Context, loaded config.Loaded, execution *lifecycleExecution, result domain.AdapterResult, runErr error, diagnostics io.Writer) error {
	if runErr != nil || result.Status != "ok" || execution.action != "start" || !execution.changed || cli.options.AdapterRunner != nil {
		return runErr
	}
	runtime, ok := cli.initPlatform(loaded, nil).(reconcileruntime.Runtime)
	if !ok {
		return nil
	}
	err := migration.CompleteRegisteredDiskTmp(ctx, runtime, func() error {
		native, _ := cli.statusPorts()
		instance, err := native.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
		if err != nil {
			return err
		}
		if instance.Config["volatile.uuid"] != execution.observed.Config["volatile.uuid"] {
			return domain.ErrPlanStale
		}
		return nil
	})
	if err == nil {
		// The completion restart follows the "started" line, so close it with its own result.
		if execution.observed.Config["user.subyard.disk_tmp"] == "" {
			native, _ := cli.statusPorts()
			actual, readErr := native.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
			if readErr == nil && actual.Config["user.subyard.disk_tmp"] == "v1" && strings.EqualFold(actual.Status, "running") {
				fmt.Fprintf(diagnostics, "  [ ok ] %s restarted; disk-backed /tmp is active (desired=running)\n", loaded.Context.YardInstanceName)
			}
		}
		return nil
	}
	failure := fmt.Errorf("yard %s: disk-/tmp completion failed: %w", loaded.Context.YardName, err)
	if ctx.Err() != nil || errors.Is(err, domain.ErrPlanStale) {
		return failure
	}
	native, _ := cli.statusPorts()
	actual, readErr := native.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if readErr == nil && actual.Config["volatile.uuid"] == execution.observed.Config["volatile.uuid"] && strings.EqualFold(actual.Status, "running") {
		fmt.Fprintf(diagnostics, "warning: yard %s: disk-/tmp completion pending: %v; run yard update --yes\n", loaded.Context.YardName, err)
		return nil
	}
	return failure
}
