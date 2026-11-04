package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func (cli *CLI) registeredYardMigrations(request releasetransition.ProcessRequest, guestTmp bool) []releasetransition.V2ActivationReconciler {
	names, err := config.YardNames(cli.options.RepositoryRoot+"/config", request.ConfigHome)
	if err != nil {
		return []releasetransition.V2ActivationReconciler{&migration.RegisteredYardMigration{Yard: "enumeration", LoadTargets: func(context.Context, releasetransition.V2SettingsSnapshotView) ([]migration.RegisteredYardTarget, error) {
			return nil, err
		}}}
	}
	result := []releasetransition.V2ActivationReconciler{}
	for _, name := range names {
		result = append(result, cli.registeredYardMigration(request, guestTmp, name))
	}
	return result
}

func (cli *CLI) registeredYardMigration(request releasetransition.ProcessRequest, guestTmp bool, yard string) releasetransition.V2ActivationReconciler {
	return &migration.RegisteredYardMigration{Yard: yard, ConfigHome: request.ConfigHome, Resources: cli.resources, GuestTmp: guestTmp,
		Retired: func(ctx context.Context, journal releasetransition.JournalRecord) (bool, error) {
			loaded, err := cli.resolveReleaseTransitionContext(yard, request.ConfigHome)
			if err != nil {
				return false, err
			}
			return cli.hardRetiredBinding(ctx, loaded, journal)
		},
		LoadTargets: func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]migration.RegisteredYardTarget, error) {
			var files *config.ProspectiveFiles
			if view != nil {
				names, err := view.ListYards()
				if err != nil {
					return nil, err
				}
				for _, name := range names {
					if !domain.SafeName(name) {
						return nil, errors.New("invalid prospective yard name")
					}
				}
				files = &config.ProspectiveFiles{Root: request.ConfigHome, Yards: names, ReadSnapshot: view.ReadSnapshot}
			}
			if yard == "test-yard" || yard == "e2e-yard" {
				warning, err := cli.releaseTestYardRecreation(ctx, request.ConfigHome, files)
				if err != nil {
					return nil, err
				}
				if warning != "" {
					return nil, nil
				}
			}
			// An older parent passes its own resolved settings; they are not overrides.
			operation := cli.releaseActivationOperation()
			loaded, err := operation.resolveReleaseTransitionContext(yard, request.ConfigHome, files)
			if err != nil {
				return nil, fmt.Errorf("yard %s: %w", yard, err)
			}
			if loaded.Context.AccessKind == domain.AccessRemote {
				return nil, nil
			}
			runtime, ok := operation.initPlatform(loaded, nil).(reconcileruntime.Runtime)
			if !ok {
				return nil, fmt.Errorf("yard %s: registered yard native runtime is unavailable", yard)
			}
			return []migration.RegisteredYardTarget{{Loaded: loaded, Runtime: runtime}}, nil
		}}
}

// Run the current transition before the strict release/ordinary-init gates.
// A missing yard or installation remains the normal fresh-init path.
func (cli *CLI) finishRegisteredInitMigrations(ctx context.Context, yard string, arguments []string, yes bool) (int, error) {
	request, err := parseInitArguments(arguments)
	if err != nil || request.mode != initReconcile || request.profile != "" || request.continueLegacy {
		return -1, err
	}
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return -1, err
	}
	if _, err := os.Stat(filepath.Join(options.RuntimeRoot, "current", "bin", "yard-engine")); errors.Is(err, os.ErrNotExist) {
		return -1, nil
	} else if err != nil {
		return -1, err
	}
	// This only selects missing compatibility work. Completed-history runtime
	// drift belongs to ordinary init and its fresh strict admission below.
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return -1, err
	}
	complete, err := releaseSourceMigrationsComplete(filepath.Join(options.RuntimeRoot, "current"), store)
	if err != nil {
		return -1, err
	}
	if complete {
		snapshot, err := store.ReadCurrentJournal()
		if err != nil {
			return -1, err
		}
		if !snapshot.Exists {
			return -1, nil
		}
		journal, err := releasetransition.ParseJournal(snapshot.Payload)
		if err != nil {
			return -1, err
		}
		if journal.Checkpoint == releasetransition.JournalComplete {
			return -1, nil
		}
	}
	loaded, err := cli.resolveReleaseTransitionContext(yard, options.ConfigHome)
	if errors.Is(err, config.ErrUnknownYard) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	native, _ := cli.statusPorts()
	if absent, ok := native.(interface{ LocalInstallationAbsent() bool }); ok && absent.LocalInstallationAbsent() {
		return -1, nil
	}
	_, err = native.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	definition, _ := cli.manifest.Lookup("migrate")
	return cli.runCurrentMigration(ctx, definition, nil, false, yes || slices.Contains(arguments, "--yes") || slices.Contains(arguments, "-y")), nil
}
