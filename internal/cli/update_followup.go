package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

type activationRepairRequiredError struct {
	inspection releasetransition.Inspection
}

func (pending *activationRepairRequiredError) Error() string {
	outcome := pending.inspection.Outcome
	return fmt.Sprintf("release %s is active; status=%s code=%s: %s; remaining plan: %s; next: run yard update to assess and authorize the remaining repair",
		outcome.Active, outcome.Status, outcome.Code, outcome.Message, strings.Join(pending.inspection.Assessment.Consequences, "; "))
}

func (execution *releaseExecution) needsActivationRepair() bool {
	inspection := execution.inspection
	if inspection == nil || inspection.Outcome == nil || len(inspection.Blockers) != 0 || inspection.Resume != nil {
		return false
	}
	outcome := inspection.Outcome
	if outcome.Status != releasetransition.StatusMigrationRequired || outcome.Code != releasetransition.CodeTransitionRequired ||
		outcome.Active != outcome.Target || string(outcome.Target) != execution.prepared.TargetRelease {
		return false
	}
	store, err := releasetransition.NewPOSIXV2Store(execution.configHome)
	if err != nil {
		return false
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return false
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	return err == nil && journal.Checkpoint == releasetransition.JournalComplete && journal.Goal.Target == outcome.Target
}

func (cli *CLI) runActivationRepair(ctx context.Context, execution *releaseExecution, assumeYes bool) int {
	options := cli.options
	// Reload persisted settings and admissions. Preserve the original command
	// inputs separately; a resolved invoking-yard snapshot is never an override.
	environment := cli.freshMigrationEnvironment(cli.baseEnv, options.RepositoryRoot)
	environment["SUBYARD_OPERATION_ID"] = newOperationID()
	options.Environment = environmentList(environment, nil)
	// This is a new same-release phase, not another release-selection request.
	// Offline preparation also prevents a missing protected target from fetching.
	root := filepath.Dir(filepath.Dir(filepath.Dir(execution.prepared.ActiveLauncher)))
	options.Arguments = []string{"-Y", execution.yard, "update", "--offline", "--version", execution.prepared.TargetVersion, "--runtime-root", root}
	if assumeYes {
		options.Arguments = append(options.Arguments, "--yes")
	}
	operation, err := New(options)
	if err != nil {
		cli.errorf("prepare activation repair: %v", err)
		return 1
	}
	operation.commandInputs = cli.commandInputs
	operation.releaseRepairTarget = releasetransition.ReleaseID(execution.prepared.TargetRelease)
	operation.promptInputTerminal = cli.promptInputTerminal
	return operation.Run(ctx)
}
