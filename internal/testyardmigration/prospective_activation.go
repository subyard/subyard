package testyardmigration

import (
	"context"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

// PrepareProjectedActivationApplicability projects only the backend creation
// mandated by Commit. Adoption/current state retains guarded native backend facts.
func PrepareProjectedActivationApplicability(ctx context.Context, options Options, before Prepared, loaded config.Loaded) (BrokerRuntimeState, State, error) {
	if err := validateOptions(&options); err != nil {
		return "", "", err
	}
	if err := validatePrepared(before); err != nil {
		return "", "", err
	}
	_, adopt, migrates := preparedLegacyState(before.State)
	if migrates && !adopt {
		broker := BrokerRuntimeInactive
		if loaded.Context.NestedE2EVMs && application.InitialPower(loaded.Context) == domain.PowerRunning {
			broker = BrokerRuntimeActive
		}
		return broker, StateCurrent, nil
	}
	if before.State == StateAbsent {
		return BrokerRuntimeAbsent, StateAbsent, nil
	}
	route, err := prepareCanonicalRouteActivationBackend(ctx, options)
	if err != nil {
		return "", "", err
	}
	broker, err := inspectBrokerRuntimeLoaded(ctx, options, CurrentYard, loaded)
	return broker.state, route, err
}
