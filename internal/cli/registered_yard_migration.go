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
			loaded, err := cli.resolveReleaseTransitionContext(ctx, yard, request.ConfigHome)
			if err != nil {
				return false, err
			}
			retired, err := cli.hardRetiredBinding(ctx, loaded, journal)
			if errors.Is(err, domain.ErrIncusAccessDenied) {
				return false, domain.IncusAccessRefusal(yard, err)
			}
			return retired, err
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
				// Recreation inspection runs the native CLI, whose socket denial
				// would surface as an untyped exit status.
				if err := cli.releaseActivationOperation().nativeAccessRefusal(ctx, yard); err != nil {
					return nil, err
				}
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
			loaded, err := operation.resolveReleaseTransitionContext(ctx, yard, request.ConfigHome, files)
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
	loaded, err := cli.resolveReleaseTransitionContext(ctx, yard, options.ConfigHome)
	if errors.Is(err, config.ErrUnknownYard) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	// A cold installation has no yard to migrate, with or without socket access.
	native, _ := cli.releaseActivationOperation().statusPorts()
	if ports.LocalInstallationAbsent(native) {
		return -1, nil
	}
	_, err = native.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return -1, nil
	}
	if errors.Is(err, domain.ErrIncusAccessDenied) {
		return -1, cli.localYardAccessRefusals(ctx, options.ConfigHome, native)
	}
	if err != nil {
		return -1, err
	}
	definition, _ := cli.manifest.Lookup("migrate")
	// Init keeps its own plan and names its own provision continuation.
	cli.profileOfferSuppressed = true
	defer func() { cli.profileOfferSuppressed = false }()
	return cli.runCurrentMigration(ctx, definition, nil, false, yes || slices.Contains(arguments, "--yes") || slices.Contains(arguments, "-y")), nil
}

// nativeAccessRefusal types a socket this account cannot open for one yard.
// An absent server or any other failure is left to the caller's own read.
func (cli *CLI) nativeAccessRefusal(ctx context.Context, yard string) error {
	native, _ := cli.statusPorts()
	if ports.LocalInstallationAbsent(native) {
		return nil
	}
	if _, err := native.Server(ctx); errors.Is(err, domain.ErrIncusAccessDenied) {
		return domain.IncusAccessRefusal(yard, err)
	}
	return nil
}

// localYardAccessRefusals reports every local yard whose native state this
// account cannot read, in YardNames order (default first, then by name).
func (cli *CLI) localYardAccessRefusals(ctx context.Context, configHome string, native interface {
	Instance(context.Context, string, string) (ports.InstanceInfo, error)
}) error {
	names, err := config.YardNames(cli.options.RepositoryRoot+"/config", configHome)
	if err != nil {
		return err
	}
	var refusals []error
	for _, name := range names {
		loaded, err := cli.resolveReleaseTransitionContext(ctx, name, configHome)
		if err != nil {
			refusals = append(refusals, fmt.Errorf("yard %s: %w", name, err))
			continue
		}
		if loaded.Context.AccessKind == domain.AccessRemote {
			continue
		}
		_, err = native.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
		if errors.Is(err, domain.ErrIncusAccessDenied) {
			refusals = append(refusals, domain.IncusAccessRefusal(name, err))
		}
	}
	return domain.CollectOrderedRefusals(refusals...)
}
