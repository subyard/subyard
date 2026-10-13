package cli

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func (cli *CLI) integrationCleanupSourceBindings(ctx context.Context, journal releasetransition.JournalRecord) (string, string, error) {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return "", "", domain.ErrPlanStale
	}
	inspector := releaseruntime.NewSourceInspector(cli.options.RepositoryRoot, cli.options.Stderr)
	defer inspector.Close()
	dir, tooling, err := inspector.InspectSourceTooling(ctx, options.RuntimeRoot, cli.options.RepositoryRoot)
	if err != nil || dir != filepath.Join(options.RuntimeRoot, "releases", string(journal.Goal.Target), "config") {
		return "", "", domain.ErrPlanStale
	}
	proof, err := cli.inspectConfigSourceAdmission(ctx, inspector)
	if err != nil || proof.Metadata.Journal == nil || proof.Metadata.Journal.Checkpoint != releasetransition.JournalComplete ||
		proof.Metadata.Journal.Transaction != journal.Transaction || proof.Metadata.Journal.ArtifactDigest != journal.ArtifactDigest {
		return "", "", domain.ErrPlanStale
	}
	return proof.Binding, tooling, nil
}

func (cli *CLI) integrationCleanupRecoveryAuthority(ctx context.Context, id string, outcome releasetransition.Outcome, journal releasetransition.JournalRecord) (string, string, error) {
	if err := cli.checkPersistentSelectionInputs(); err != nil {
		return "", "", err
	}
	if !domain.SafeName(id) || outcome.Status != releasetransition.StatusOperatorActionRequired || outcome.Code != releasetransition.CodeDependencyUnavailable ||
		outcome.ReachedGoal || outcome.Transaction == nil || *outcome.Transaction != journal.Transaction ||
		!configApplyRepairJournalMatches(journal, outcome) {
		return "", "", domain.ErrPlanStale
	}
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return "", "", domain.ErrPlanStale
	}
	runtime := releaseruntime.New(releaseruntime.Config{Inputs: cli.commandInputs})
	defer runtime.Close()
	inspection, err := runtime.InspectMutationGateInspection(ctx, options.RuntimeRoot, options.ConfigHome, "default")
	if err != nil || inspection == nil || inspection.Outcome == nil || !sameConfigApplyGate(outcome, *inspection.Outcome) ||
		len(inspection.Blockers) != 1 || inspection.Blockers[0].Resource != "activation.materialized-config" ||
		inspection.Blockers[0].Code != releasetransition.CodeDependencyUnavailable {
		return "", "", domain.ErrPlanStale
	}
	return cli.integrationCleanupSourceBindings(ctx, journal)
}

func (cli *CLI) prepareIntegrationCleanupRecovery(ctx context.Context, yard string, arguments []string, outcome releasetransition.Outcome) (*configApplyRepairPermit, error) {
	request, err := parseIntegrationArguments(arguments)
	if err != nil || request.verb != "cleanup" || request.check {
		return nil, err
	}
	// Capture the same controller identity and adopted context as ordinary preparation.
	cli.ensureOperationID()
	loaded, err := cli.loadContext(yard)
	if err != nil || loaded.Context.AccessKind != domain.AccessLocal {
		return nil, errors.New("integration cleanup recovery requires the local source owner")
	}
	if err := cli.requireRunningIntegrationYard(ctx, loaded); err != nil {
		return nil, err
	}
	return cli.prepareActivationRepair(ctx, yard, false, outcome, false, nil, false, request.id)
}

func (cli *CLI) finishIntegrationCleanupRecovery(ctx context.Context, permit *configApplyRepairPermit) error {
	journal, err := releasetransition.ParseJournal(permit.journal.Payload)
	if err != nil {
		return domain.ErrPlanStale
	}
	source, tooling, err := cli.integrationCleanupSourceBindings(ctx, journal)
	if err != nil || source != permit.sourceBinding || tooling != permit.toolingBinding {
		return domain.ErrPlanStale
	}
	return nil
}
