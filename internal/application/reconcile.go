package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type ReconcileStage struct {
	ID    ports.ReconcileStageID
	Label string
}

type ReconcileStep struct {
	Stage     ReconcileStage
	Converged bool
}

type ReconcilePlan struct {
	Steps []ReconcileStep
}

func (plan ReconcilePlan) Pending() int {
	pending := 0
	for _, step := range plan.Steps {
		if !step.Converged {
			pending++
		}
	}
	return pending
}

type ReconcileReporter interface {
	StageSkipped(ReconcileStage)
	StageStarted(ReconcileStage)
}

type Reconciler struct {
	Stages   []ReconcileStage
	Runner   ports.ReconcileStageRunner
	Reporter ReconcileReporter
}

func (reconciler Reconciler) Plan(ctx context.Context) (ReconcilePlan, error) {
	if err := validateStages(reconciler.Stages); err != nil {
		return ReconcilePlan{}, err
	}
	if reconciler.Runner == nil {
		return ReconcilePlan{}, errors.New("reconcile stage runner is required")
	}
	plan := ReconcilePlan{Steps: make([]ReconcileStep, 0, len(reconciler.Stages))}
	for index, stage := range reconciler.Stages {
		converged, err := reconciler.Runner.CheckStage(ctx, stage.ID)
		if err != nil {
			return ReconcilePlan{}, fmt.Errorf("check init stage %q: %w", stage.ID, err)
		}
		plan.Steps = append(plan.Steps, ReconcileStep{Stage: stage, Converged: converged})
		if !converged {
			for _, dependent := range reconciler.Stages[index+1:] {
				plan.Steps = append(plan.Steps, ReconcileStep{Stage: dependent})
			}
			break
		}
	}
	return plan, nil
}

func (reconciler Reconciler) Apply(ctx context.Context) error {
	if err := validateStages(reconciler.Stages); err != nil {
		return err
	}
	if reconciler.Runner == nil {
		return errors.New("reconcile stage runner is required")
	}
	for _, stage := range reconciler.Stages {
		converged, err := reconciler.Runner.CheckStage(ctx, stage.ID)
		if err != nil {
			return fmt.Errorf("check init stage %q: %w", stage.ID, err)
		}
		if converged {
			if reconciler.Reporter != nil {
				reconciler.Reporter.StageSkipped(stage)
			}
			continue
		}
		if reconciler.Reporter != nil {
			reconciler.Reporter.StageStarted(stage)
		}
		if err := reconciler.Runner.ApplyStage(ctx, stage.ID); err != nil {
			return fmt.Errorf("apply init stage %q: %w", stage.ID, err)
		}
		verified, err := reconciler.Runner.VerifyStage(ctx, stage.ID)
		if err != nil {
			return fmt.Errorf("verify init stage %q: %w", stage.ID, err)
		}
		if !verified {
			return fmt.Errorf("init stage %q completed but did not converge: %s", stage.ID, stage.Label)
		}
	}
	return nil
}

func InitStages(yard domain.Context) []ReconcileStage {
	instance := "Create the yard instance (+ /dev/kvm, /srv volume)"
	if yard.NestedE2EVMs {
		instance = "Create the yard instance (+ trusted nested-VM KVM/vsock/BPF boundary, /srv volume)"
	}
	testVMs := "Keep the nested VM test backend disabled"
	if yard.NestedE2EVMs {
		testVMs = "Install/reconcile the trusted two-VM test backend inside the yard"
	}
	return []ReconcileStage{
		{ID: ports.ReconcileStageIncus, Label: "Install or upgrade Incus and initialize storage"},
		{ID: ports.ReconcileStageProject, Label: fmt.Sprintf("Create the Incus project %q", yard.IncusProject)},
		{ID: ports.ReconcileStageNetwork, Label: "Open host DHCP/DNS for the yard bridge"},
		{ID: ports.ReconcileStageNetworkPolicy,
			Label: "Reconcile same-host yard network policy (may restart affected running yards when isolation is enabled)"},
		{ID: ports.ReconcileStagePowerImport, Label: "Import desired-power state for registered local yards"},
		{ID: ports.ReconcileStageInstance, Label: instance},
		{ID: ports.ReconcileStageMounts, Label: fmt.Sprintf("Create host dirs under %s and mount them", yard.Paths.HostBase)},
		{ID: ports.ReconcileStageProvision, Label: "Provision the yard"},
		{ID: ports.ReconcileStageTestVMs, Label: testVMs},
		{ID: ports.ReconcileStageSSH, Label: "Set up SSH access into the yard"},
		{ID: ports.ReconcileStageGitHub, Label: "Reconcile the GitHub broker profile"},
		{ID: ports.ReconcileStageGitIdentity, Label: "Reconcile in-yard git config and bind-worktree trust"},
		{ID: ports.ReconcileStageExtras, Label: "Apply yard extras requested by projects"},
		{ID: ports.ReconcileStagePower, Label: "Persist desired yard power and install host boot reconciliation"},
		{ID: ports.ReconcileStageKeys, Label: "Initialize the encrypted credential ledger and sync timer"},
		{ID: ports.ReconcileStageSecurity, Label: "Validate host-boundary security invariants"},
	}
}

func FinalizeStage() ReconcileStage {
	return ReconcileStage{
		ID:    ports.ReconcileStageFinalize,
		Label: "Restore and commit the configured desired yard power state",
	}
}

func validateStages(stages []ReconcileStage) error {
	if len(stages) == 0 {
		return errors.New("reconcile stage registry is empty")
	}
	seen := make(map[ports.ReconcileStageID]struct{}, len(stages))
	for _, stage := range stages {
		if !domain.SafeName(string(stage.ID)) || stage.Label == "" {
			return fmt.Errorf("invalid reconcile stage %q", stage.ID)
		}
		if _, duplicate := seen[stage.ID]; duplicate {
			return fmt.Errorf("duplicate reconcile stage %q", stage.ID)
		}
		seen[stage.ID] = struct{}{}
	}
	return nil
}
