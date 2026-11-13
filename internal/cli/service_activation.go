package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerservice"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// Bind only new-release activation. Readiness, same-release repair and config
// gates retain the native owner, including every optional interface.
func (cli *CLI) bindServiceRegistrationActivation(request releasetransition.ProcessRequest, reconcilers []releasetransition.V2ActivationReconciler) []releasetransition.V2ActivationReconciler {
	links, err := releasetransition.NewRuntimeLinkStore(request.RuntimeRoot)
	if err != nil {
		return reconcilers
	}
	active, err := links.Observe()
	if err != nil || request.Direction != releasetransition.DirectionActivateTarget {
		return reconcilers
	}
	if active.Active == request.Target {
		// After activation, only the exact incomplete cross-release journal
		// proves this is a new-release resume rather than same-release repair.
		store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
		if err != nil {
			return reconcilers
		}
		snapshot, err := store.ReadCurrentJournal()
		if err != nil || !snapshot.Exists {
			return reconcilers
		}
		journal, err := releasetransition.ParseJournal(snapshot.Payload)
		if err != nil || journal.Checkpoint == releasetransition.JournalComplete || journal.Goal != (releasetransition.Goal{Target: request.Target, Direction: request.Direction}) || journal.Releases.From == journal.Releases.Target {
			return reconcilers
		}
	}
	for _, definition := range cli.catalog.Profiles() {
		service := definition.InfrastructureService
		if service == nil || service.RegistrationActivation == "" {
			continue
		}
		for _, owner := range reconcilers {
			stage, ok := owner.(*activationStageReconciler)
			if !ok || stage.ID() != service.RegistrationActivation {
				continue
			}
			stage.planned = func() []string {
				home, _, _, err := cli.servicePaths()
				if err != nil {
					return nil
				}
				records, err := (ownerservice.Registry{Root: filepath.Join(home, "services")}).List(service.ID)
				if err != nil || len(records) != 0 {
					return nil
				}
				names, _ := config.YardNames(filepath.Join(cli.options.RepositoryRoot, "config"), request.ConfigHome)
				for _, name := range names {
					loaded, err := cli.resolveReleaseTransitionContext(context.Background(), name, request.ConfigHome, nil)
					if err == nil && loaded.Context.AccessKind == domain.AccessLocal && service.Matches(loaded.Environment) {
						return []string{fmt.Sprintf("register service %s (pool default) when exactly one owned runtime is proven; runtime and leases retained", service.ID)}
					}
				}
				return nil
			}
			stage.after = func(ctx context.Context) error {
				home, _, _, err := cli.servicePaths()
				if err != nil {
					fmt.Fprintf(cli.options.Stderr, "Diagnostic: service %s registration: %v; run yard service register %s --yard NAME\n", service.ID, err, service.ID)
					return nil
				}
				// Historical callers may not pass an owner barrier. OwnerLock also
				// verifies and borrows an inherited descriptor without unlocking it.
				bounded, cancel := context.WithTimeout(ctx, time.Second)
				_, unlock, err := uninstall.OwnerLock(bounded, home, nil)
				cancel()
				if err != nil {
					fmt.Fprintf(cli.options.Stderr, "Diagnostic: service %s registration: %v; run yard service register %s --yard NAME\n", service.ID, err, service.ID)
					return nil
				}
				defer unlock()
				registry := ownerservice.Registry{Root: filepath.Join(home, "services")}
				registry.RegisterActivation(service.ID, filepath.Join(home, service.RouteRoot, "services"), func() ([]ownerservice.ActivationCandidate, bool, error) {
					_, _, installation, err := cli.servicePaths()
					if err != nil {
						return nil, false, err
					}
					names, err := config.YardNames(filepath.Join(cli.options.RepositoryRoot, "config"), request.ConfigHome)
					if err != nil {
						return nil, false, err
					}
					var candidates []ownerservice.ActivationCandidate
					var findings []error
					selected := false
					for _, name := range names {
						loaded, err := cli.resolveReleaseTransitionContext(ctx, name, request.ConfigHome, nil)
						if err != nil {
							// Its role is unknown, so report the failed read separately
							// and withhold registration without inventing a candidate.
							findings = append(findings, fmt.Errorf("yard %s: load configuration: %w", name, err))
							continue
						}
						if loaded.Context.AccessKind != domain.AccessLocal || !service.Matches(loaded.Environment) {
							continue
						}
						selected = true
						incus, _ := cli.statusPorts()
						state, err := incus.ReconcileState(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName, loaded.Context.IncusBridge, loaded.Environment["SRV_POOL"], loaded.Environment["SRV_VOLUME"])
						if err == nil && !state.InstanceFound {
							continue
						}
						candidate := ownerservice.ActivationCandidate{Registration: ownerservice.Registration{Public: ownerservice.Public{Owner: installation, Yard: name, Project: loaded.Context.IncusProject, Instance: loaded.Context.YardInstanceName}}}
						if err == nil {
							candidate.UUID, candidate.Ownership, err = cli.serviceRuntime(ctx, loaded, definition)
						}
						candidate.Err = err
						candidates = append(candidates, candidate)
					}
					return candidates, selected, domain.CollectRefusals(findings...)
				}, cli.options.Stderr)
				return nil
			}
		}
	}
	return reconcilers
}
