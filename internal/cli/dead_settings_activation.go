package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// This settings migration is an idempotent activation owner: it runs even when
// the previous release already advanced every settings epoch. It changes no
// runtime format and needs no rollback boundary.
type deadSettingsActivation struct {
	cli  *CLI
	home string
	plan config.DeadAssignmentPlan
}

func (*deadSettingsActivation) ID() string { return "legacy-settings" }

func (owner *deadSettingsActivation) activationConfiguration() *CLI { return owner.cli }

func (owner *deadSettingsActivation) Observe(_ context.Context, _ releasetransition.ReleasePair, _ releasetransition.ReleaseLinks) (releasetransition.V2ActivationObservation, error) {
	return owner.observe(nil)
}

func (owner *deadSettingsActivation) ObserveProspective(_ context.Context, _ releasetransition.ReleasePair, _ releasetransition.ReleaseLinks, view releasetransition.V2SettingsSnapshotView) (releasetransition.V2ActivationObservation, error) {
	if view == nil {
		return owner.observe(nil)
	}
	return owner.observe(&config.ProspectiveFiles{Root: owner.home, ReadSnapshot: view.ReadSnapshot})
}

func (owner *deadSettingsActivation) observe(files *config.ProspectiveFiles) (releasetransition.V2ActivationObservation, error) {
	plan, err := owner.cli.catalog.PlanDeadAssignments(owner.home, filepath.Join(owner.cli.options.RepositoryRoot, "config"), files)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, domain.CollectRefusals(err)
	}
	// Keep ordinary expansion refusals, including other independent lines.
	_, err = config.ParsePersistentAssignments(plan.Path, plan.Preview())
	if err != nil {
		return releasetransition.V2ActivationObservation{}, domain.CollectRefusals(err)
	}
	owner.plan = plan
	actual := sha256.Sum256(plan.Expected.Content)
	desired := sha256.Sum256(plan.Desired)
	binding, err := activationStageFingerprint(plan)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	observation := releasetransition.V2ActivationObservation{Actual: releasetransition.Fingerprint(fmt.Sprintf("%x", actual)), Desired: releasetransition.Fingerprint(fmt.Sprintf("%x", desired)), Binding: binding, Converged: len(plan.Removed) == 0}
	for _, assignment := range plan.Removed {
		observation.Consequences = append(observation.Consequences, assignment.String())
	}
	return observation, nil
}

func (owner *deadSettingsActivation) Reconcile(_ context.Context, _ releasetransition.ReleaseLinks) error {
	if err := owner.plan.Apply(owner.home); err != nil {
		return err
	}
	for _, assignment := range owner.plan.Removed {
		fmt.Fprintf(owner.cli.options.Stdout, "%s:%d: removed dead %s assignment; file-settings source %s takes precedence\n", assignment.Path, assignment.Line, assignment.Name, assignment.Source)
	}
	return nil
}

func (owner *deadSettingsActivation) PrepareActivationRecovery(ctx context.Context, releases releasetransition.ReleasePair, links releasetransition.ReleaseLinks) (releasetransition.Fingerprint, error) {
	observation, err := owner.Observe(ctx, releases, links)
	return observation.Binding, err
}
