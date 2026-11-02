package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"

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
	prompt := migrationLatePrompt{cli: cli}
	return &migration.RegisteredYardMigration{Yard: yard, ConfigHome: request.ConfigHome, Resources: cli.resources, GuestTmp: guestTmp, Prompt: prompt,
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
			loaded, err := cli.resolveReleaseTransitionContext(yard, request.ConfigHome, files)
			if err != nil {
				return nil, err
			}
			if loaded.Context.AccessKind == domain.AccessRemote {
				return nil, nil
			}
			runtime, ok := cli.initPlatform(loaded, nil).(reconcileruntime.Runtime)
			if !ok {
				return nil, errors.New("registered yard native runtime is unavailable")
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

type migrationLatePrompt struct{ cli *CLI }

func migrationOperatorInputDescriptor(value string) (int, bool) {
	fd, err := strconv.Atoi(value)
	return fd, err == nil && fd >= 3 && fd <= 64
}

func (prompt migrationLatePrompt) Confirm(ctx context.Context, request domain.ConfirmationRequest) (bool, error) {
	cli := prompt.cli
	if cli.migrationOperatorInput == nil {
		value, provided := os.LookupEnv("SUBYARD_MIGRATION_OPERATOR_INPUT_FD")
		if provided {
			fd, valid := migrationOperatorInputDescriptor(value)
			if !valid {
				return false, domain.ErrConfirmationRequired
			}
			cli.migrationOperatorInput = os.NewFile(uintptr(fd), "migration-operator-input")
		} else {
			if cli.openTerminal == nil {
				return false, domain.ErrConfirmationRequired
			}
			input, err := cli.openTerminal()
			if err != nil {
				return false, domain.ErrConfirmationRequired
			}
			cli.migrationOperatorInput = input
		}
	}
	input := cli.migrationOperatorInput
	return (streamPrompt{input: input, output: cli.options.Stderr, interactive: func() bool { return terminalStream(input) }}).Confirm(ctx, request)
}
