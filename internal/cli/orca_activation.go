package cli

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"sort"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// Refresh only installed profile files; the profile adapter owns their contract.
// The transition owns confirmation, recovery, and verification across local yards.
type orcaRuntimeActivationReconciler struct {
	cli     *CLI
	request releasetransition.ProcessRequest
}

func (cli *CLI) orcaActivationReconciler(request releasetransition.ProcessRequest) releasetransition.V2ActivationReconciler {
	return &orcaRuntimeActivationReconciler{cli: cli, request: request}
}

func (*orcaRuntimeActivationReconciler) ID() string { return "orca-runtime" }

func (reconciler *orcaRuntimeActivationReconciler) targets() (*CLI, []configTarget, error) {
	operation := *reconciler.cli
	environment := freshMigrationEnvironment(operation.baseEnv, operation.options.RepositoryRoot)
	environment["SUBYARD_OPERATION_ID"] = operation.env["SUBYARD_OPERATION_ID"]
	operation.baseEnv, operation.env = environment, maps.Clone(environment)
	yard := reconciler.request.Yard
	if yard == "" {
		yard = "default"
	}
	loaded, err := operation.resolveReleaseTransitionContext(yard, reconciler.request.ConfigHome)
	if err != nil {
		return nil, nil, err
	}
	targets, err := operation.localConfigTargets(loaded, true)
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return &operation, targets, err
}

func (reconciler *orcaRuntimeActivationReconciler) sourceMigrationsComplete() (bool, error) {
	store, err := releasetransition.NewPOSIXV2Store(reconciler.request.ConfigHome)
	if err != nil {
		return false, err
	}
	return releaseSourceMigrationsComplete(reconciler.cli.options.RepositoryRoot, store)
}

func (reconciler *orcaRuntimeActivationReconciler) desired(
	releases releasetransition.ReleasePair,
) (releasetransition.Fingerprint, error) {
	return activationStageFingerprint(struct {
		ID       string                        `json:"id"`
		Target   releasetransition.ReleaseID   `json:"target"`
		Artifact releasetransition.Fingerprint `json:"artifact"`
	}{ID: reconciler.ID(), Target: releases.Target, Artifact: reconciler.request.ArtifactDigest})
}

func orcaActivationPlatform(operation *CLI, target configTarget) ports.InitPlatform {
	return operation.initPlatformWithDispatcher(target.Loaded, nil,
		filepath.Join(operation.options.RepositoryRoot, "bin", "yard-engine"))
}

func (reconciler *orcaRuntimeActivationReconciler) Observe(
	ctx context.Context, releases releasetransition.ReleasePair, _ releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	desiredDigest, err := reconciler.desired(releases)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	complete, err := reconciler.sourceMigrationsComplete()
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	if !complete {
		actual, err := activationStageFingerprint(struct {
			ID    string `json:"id"`
			State string `json:"state"`
		}{ID: reconciler.ID(), State: "source-migrations-pending"})
		return releasetransition.V2ActivationObservation{
			Actual: actual, Desired: desiredDigest, Converged: false,
		}, err
	}
	operation, targets, err := reconciler.targets()
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	type observation struct {
		Yard, Project, Instance, State, Digest string
	}
	actual := make([]observation, 0, len(targets))
	converged := true
	var warnings []string
	for _, target := range targets {
		state, err := orcaActivationPlatform(operation, target).ObserveOrcaRuntime(ctx)
		if err != nil {
			return releasetransition.V2ActivationObservation{}, fmt.Errorf("yard %s Orca runtime: %w", target.Name, err)
		}
		kind := state.State
		if kind == "current" || kind == "stale" {
			kind = "installed"
		}
		entry := observation{Yard: target.Name, Project: target.Loaded.Context.IncusProject,
			Instance: target.Loaded.Context.YardInstanceName, State: kind, Digest: state.Actual}
		actual = append(actual, entry)
		converged = converged && state.State != "stale"
		if state.State == "deferred" {
			warnings = append(warnings, fmt.Sprintf(
				"yard %s: installed Orca handler refresh deferred while the yard is stopped; run init after starting it", target.Name))
		}
	}
	actualDigest := desiredDigest
	if !converged {
		actualDigest, err = activationStageFingerprint(actual)
	}
	return releasetransition.V2ActivationObservation{Actual: actualDigest, Desired: desiredDigest, Converged: converged, Warnings: warnings}, err
}

func (reconciler *orcaRuntimeActivationReconciler) Reconcile(ctx context.Context, _ releasetransition.ReleaseLinks) error {
	complete, err := reconciler.sourceMigrationsComplete()
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("Orca runtime refresh requires completed source migrations")
	}
	operation, targets, err := reconciler.targets()
	if err != nil {
		return err
	}
	for _, target := range targets {
		platform := orcaActivationPlatform(operation, target)
		state, err := platform.ObserveOrcaRuntime(ctx)
		if err != nil {
			return fmt.Errorf("yard %s Orca runtime: %w", target.Name, err)
		}
		if state.State != "stale" {
			continue
		}
		if err := platform.ApplyStage(ctx, ports.ReconcileStageOrca); err != nil {
			return fmt.Errorf("yard %s Orca runtime refresh: %w", target.Name, err)
		}
		state, err = platform.ObserveOrcaRuntime(ctx)
		if err != nil {
			return err
		}
		if state.State != "current" {
			return fmt.Errorf("yard %s Orca runtime did not converge", target.Name)
		}
	}
	return nil
}
