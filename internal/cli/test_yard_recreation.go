package cli

import (
	"context"
	"errors"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func testYardRecreation(ctx context.Context, options testyardmigration.Options) (string, error) {
	state, err := testyardmigration.RecreationState(ctx, options)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		var conflict interface{ ActivationConflict() bool }
		if !errors.As(err, &conflict) || !conflict.ActivationConflict() {
			return "", err
		}
	} else if state == testyardmigration.StateAbsent || state == testyardmigration.StateCurrent {
		return "", nil
	}
	return testyardmigration.RecreationHint(options, state != testyardmigration.StateCurrent)
}

func (cli *CLI) releaseTestYardRecreation(ctx context.Context, home string, files *config.ProspectiveFiles) (string, error) {
	options, err := cli.releaseTransitionTestYardOptions(releasetransition.ProcessRequest{ConfigHome: home})
	if err != nil {
		return "", err
	}
	options.ProspectiveFiles = files
	return testYardRecreation(ctx, options)
}

func (cli *CLI) releaseConfigTargets(ctx context.Context, loaded config.Loaded, allLocal bool) ([]configTarget, error) {
	targets, _, err := cli.configTargets(ctx, loaded, allLocal, true)
	return targets, err
}
