package cli

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

// Physical session and owner-service guards address the installed account.
// Provisioning, resource scope and desired configuration retain their own inputs.
type physicalOwner struct {
	User     string
	UID      int
	Profiles string
}

// An unreadable applied identity keeps the desired context, as before this
// projection existed, so an unresponsive or damaged guest cannot block stop,
// teardown or a resource shutdown. The read is bounded for the same reason.
func (cli *CLI) physicalOwner(ctx context.Context, loaded config.Loaded) physicalOwner {
	bounded, cancel := context.WithTimeout(ctx, physicalOwnerTimeout)
	defer cancel()
	target, err := cli.appliedIntegrationTarget(bounded, configTarget{Name: loaded.Context.YardName, Loaded: loaded})
	if err != nil {
		target.Loaded = loaded
	}
	return physicalOwner{target.Loaded.Context.DevUser, target.Loaded.Context.DevUID, target.Loaded.Environment["ENVIRONMENT_PROFILES"]}
}

const physicalOwnerTimeout = 30 * time.Second

func (owner physicalOwner) apply(values map[string]string) {
	values["DEV_USER"], values["DEV_UID"] = owner.User, strconv.Itoa(owner.UID)
	values["ENVIRONMENT_PROFILES"] = owner.Profiles
}

// resourceShutdownContext addresses the account a resource runtime runs as, which
// differs from the desired one until init applies a rename.
func (cli *CLI) resourceShutdownContext(ctx context.Context, loaded config.Loaded, definition resource.Definition, verb string) config.Loaded {
	if verb != definition.Shutdown {
		return loaded
	}
	return cli.withInstalledAccount(ctx, loaded)
}

func (cli *CLI) withInstalledAccount(ctx context.Context, loaded config.Loaded) config.Loaded {
	owner := cli.physicalOwner(ctx, loaded)
	if owner.User == loaded.Context.DevUser {
		return loaded
	}
	loaded.Environment = maps.Clone(loaded.Environment)
	loaded.Environment["DEV_USER"], loaded.Environment["DEV_UID"] = owner.User, strconv.Itoa(owner.UID)
	loaded.Context.DevUser, loaded.Context.DevUID = owner.User, owner.UID
	return loaded
}

func (cli *CLI) bindLifecycleOwner(ctx context.Context, loaded config.Loaded, execution *lifecycleExecution) error {
	if execution.action != "stop" || !execution.changed {
		return nil
	}
	owner := cli.physicalOwner(ctx, loaded)
	if execution.owner != nil && *execution.owner != owner {
		return fmt.Errorf("%w: installed lifecycle owner changed", domain.ErrPlanStale)
	}
	execution.owner = &owner
	return nil
}

const teardownOwnerUser = "SUBYARD_TEARDOWN_DEV_USER"
const teardownOwnerUID = "SUBYARD_TEARDOWN_DEV_UID"
const teardownOwnerProfiles = "SUBYARD_TEARDOWN_ENVIRONMENT_PROFILES"

func (cli *CLI) bindTeardownOwner(ctx context.Context, loaded config.Loaded, execution *teardownExecution) error {
	incus, _ := cli.statusPorts()
	instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if err != nil || !strings.EqualFold(instance.Status, "running") {
		return nil // Existing teardown guards own absent, stopped and unreachable guests.
	}
	values := execution.activeYard.Environment
	user, recorded := values[teardownOwnerUser]
	profiles, profilesRecorded := values[teardownOwnerProfiles]
	if !recorded && execution.snapshot() != nil {
		return nil // A resumed legacy plan did not approve an owner projection.
	}
	owner := cli.physicalOwner(ctx, loaded)
	if recorded && (user != owner.User || values[teardownOwnerUID] != strconv.Itoa(owner.UID) || profilesRecorded && profiles != owner.Profiles) {
		return fmt.Errorf("%w: installed teardown owner changed", domain.ErrPlanStale)
	}
	values[teardownOwnerUser], values[teardownOwnerUID] = owner.User, strconv.Itoa(owner.UID)
	// A resumed legacy snapshot did not approve a profile projection. Preserve
	// its original environment rather than adding new scope during recovery.
	if !recorded || profilesRecorded {
		values[teardownOwnerProfiles] = owner.Profiles
	}
	return nil
}

func applyTeardownOwner(values map[string]string, execution *teardownExecution) {
	if user, ok := execution.activeYard.Environment[teardownOwnerUser]; ok {
		values["DEV_USER"], values["DEV_UID"] = user, execution.activeYard.Environment[teardownOwnerUID]
		if profiles, known := execution.activeYard.Environment[teardownOwnerProfiles]; known {
			values["ENVIRONMENT_PROFILES"] = profiles
		}
	}
}
