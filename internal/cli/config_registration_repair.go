package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

func configRegistrationRepairInvocation(arguments []string) bool {
	if len(arguments) > 0 && (arguments[0] == "--yes" || arguments[0] == "-y") {
		arguments = arguments[1:]
	}
	return len(arguments) > 0 && arguments[0] == "repair-registration"
}

func (cli *CLI) runConfigRegistrationRepair(
	ctx context.Context, loaded config.Loaded, arguments []string, assumeYes bool,
) int {
	if loaded.Context.AccessKind == domain.AccessRemote {
		cli.errorf("config repair-registration must run on the registration owner")
		return 2
	}
	name, check := "", false
	for _, argument := range arguments {
		switch argument {
		case "--check":
			check = true
		case "--yes", "-y":
			assumeYes = true
		default:
			if name != "" || strings.HasPrefix(argument, "-") {
				cli.errorf("config repair-registration expects one yard name, --check or --yes")
				return 2
			}
			name = argument
		}
	}
	store, err := releasetransition.NewPOSIXV2Store(loaded.Context.Paths.ConfigHome)
	if err != nil {
		cli.errorf("config repair-registration: %v", err)
		return 1
	}
	before, err := cli.inspectRegistrationRepairGate(ctx, store, loaded.Context.YardName)
	if err != nil {
		cli.errorf("config repair-registration: %v", err)
		return 1
	}
	plan, err := config.PlanYardRegistrationRepair(loaded.Context.Paths.ConfigHome, name)
	if err != nil {
		cli.errorf("config repair-registration: %v", err)
		return 1
	}
	if !plan.Changed() {
		fmt.Fprintln(cli.options.Stdout, "Yard registration is already unambiguous.")
		return 0
	}
	consequence := fmt.Sprintf("Keep %s active; preserve %s at %s.", plan.NestedPath, plan.FlatPath, plan.ArchivePath)
	fmt.Fprintln(cli.options.Stdout, consequence)
	if check {
		return 0
	}
	if !cli.planConfigAction(ctx, loaded, "repair-registration", assumeYes, false, consequence) {
		return 1
	}
	unlock, err := store.Lock()
	if err != nil {
		cli.errorf("config repair-registration: %v", err)
		return 1
	}
	defer unlock()
	after, err := cli.inspectRegistrationRepairGate(ctx, store, loaded.Context.YardName)
	if err == nil && (before.Exists != after.Exists || before.Fingerprint != after.Fingerprint) {
		err = domain.ErrPlanStale
	}
	if err == nil {
		err = plan.Apply()
	}
	if errors.Is(err, config.ErrPersistentTargetStale) {
		err = fmt.Errorf("%w: yard registration changed after confirmation", domain.ErrPlanStale)
	}
	if err != nil {
		cli.errorf("config repair-registration: %v", err)
		return 1
	}
	fmt.Fprintln(cli.options.Stdout, "Yard registration repaired; run yard update --check again.")
	return 0
}

// Repair may resolve a precondition on completed history, but must never alter
// configuration bound to an unfinished release transition.
func (cli *CLI) inspectRegistrationRepairGate(
	ctx context.Context, store *releasetransition.POSIXV2Store, yard string,
) (releasetransition.ProtectedSnapshot, error) {
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return snapshot, err
	}
	if snapshot.Exists {
		journal, err := releasetransition.ParseJournal(snapshot.Payload)
		if err != nil {
			return snapshot, err
		}
		if journal.Checkpoint != releasetransition.JournalComplete {
			return snapshot, errors.New("finish the pending release transition before repairing yard registrations")
		}
	}
	outcome, err := cli.inspectMutationGate(ctx, yard)
	if err != nil {
		return snapshot, err
	}
	if outcome != nil && !(snapshot.Exists && outcome.Code == releasetransition.CodePreconditionBlocked) {
		return snapshot, fmt.Errorf("release transition %s: %s", outcome.Code, outcome.Message)
	}
	// A completed V2 blocker can mask the legacy gate in the ordinary dispatcher.
	// Both journal families must permit this independent configuration repair.
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil {
		return snapshot, err
	}
	if available {
		legacy, err := migration.InspectMutationGate(ctx, options)
		if err != nil {
			return snapshot, err
		}
		if legacy != nil {
			return snapshot, fmt.Errorf("legacy release transition %s: %s", legacy.Code, legacy.Message)
		}
	}
	return snapshot, nil
}
