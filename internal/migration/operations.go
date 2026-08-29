package migration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/testyardmigration"
)

const (
	operationPrepared    = "prepared"
	operationCommitting  = "committing"
	operationCommitted   = "committed"
	operationRollingBack = "rolling-back"
	operationRolledBack  = "rolled-back"
)

func verifyTypedOperation(
	ctx context.Context,
	options ReleaseOptions,
	operation Operation,
	before string,
) error {
	switch operation.Kind {
	case OperationKindTestYardOwnerV1:
		return testyardmigration.VerifyState(testYardOptions(options), testyardmigration.State(before))
	case OperationKindTestYardRouteConsumersV1:
		return testyardmigration.VerifyRouteConsumers(ctx, testYardOptions(options), before)
	case OperationKindTestVMBrokerRuntimeV1:
		return testyardmigration.VerifyBrokerRuntime(
			ctx, testYardOptions(options), testyardmigration.BrokerRuntimeState(before),
		)
	case OperationKindPowerReconcilerRuntimeV1,
		OperationKindPowerReconcilerSystemdCompatV1:
		return verifyPowerReconciler(ctx, options, before)
	default:
		return fmt.Errorf("unsupported migration operation kind %q", operation.Kind)
	}
}

func verifyTypedOperationRollback(
	ctx context.Context,
	options ReleaseOptions,
	operation Operation,
	before string,
	fromLayout int,
) error {
	switch operation.Kind {
	case OperationKindTestYardOwnerV1:
		return testyardmigration.VerifyRollback(testYardOptions(options), testyardmigration.State(before))
	case OperationKindTestYardRouteConsumersV1:
		return testyardmigration.VerifyRouteConsumersRollback(ctx, testYardOptions(options), before)
	case OperationKindTestVMBrokerRuntimeV1:
		if err := validateBrokerRollbackLayout(fromLayout); err != nil {
			return fmt.Errorf("operation %q: %w", operation.ID, err)
		}
		previous, err := previousRuntimeOptions(options)
		if err != nil {
			return err
		}
		return testyardmigration.VerifyBrokerRuntime(
			ctx, testYardOptions(previous), testyardmigration.BrokerRuntimeState(before),
		)
	case OperationKindPowerReconcilerRuntimeV1,
		OperationKindPowerReconcilerSystemdCompatV1:
		if before == powerReconcilerAbsent {
			return verifyPowerReconciler(ctx, options, before)
		}
		previous, err := previousRuntimeOptions(options)
		if err != nil {
			return err
		}
		previous.Environment = powerReconcilerEnvironment(previous, previous.RepositoryRoot)
		if operation.Kind == OperationKindPowerReconcilerSystemdCompatV1 && fromLayout == 4 {
			return verifyRestoredPowerReconciler(ctx, previous, before)
		}
		return verifyPowerReconciler(ctx, previous, before)
	default:
		return fmt.Errorf("unsupported migration operation kind %q", operation.Kind)
	}
}

func validateBrokerRollbackLayout(fromLayout int) error {
	if fromLayout < 1 {
		return fmt.Errorf("unsupported broker rollback layout %d", fromLayout)
	}
	return nil
}

func testYardOptions(options ReleaseOptions) testyardmigration.Options {
	executable := options.Executable
	if executable == "" {
		executable = filepath.Join(options.RepositoryRoot, "bin", "yard-engine")
	}
	environment := options.Environment
	if environment == nil {
		environment = os.Environ()
	}
	diagnostics := options.Diagnostics
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	return testyardmigration.Options{
		Executable: executable, RepositoryRoot: options.RepositoryRoot,
		Incus:      options.Incus,
		ConfigHome: options.ConfigHome, DataHome: options.DataHome,
		Environment: environment, Stdout: diagnostics, Stderr: stderr,
	}
}

func validateOperationState(operation transactionOperation) error {
	if operation.Before == "" {
		return errors.New("typed migration operation has no prepared state")
	}
	switch operation.Kind {
	case OperationKindTestYardOwnerV1:
		switch testyardmigration.State(operation.Before) {
		case testyardmigration.StateAbsent,
			testyardmigration.StateLegacyDirectory,
			testyardmigration.StateLegacyDirectoryProjects,
			testyardmigration.StateLegacyDirectoryOverrides,
			testyardmigration.StateLegacyDirectoryState,
			testyardmigration.StateLegacyFlat,
			testyardmigration.StateLegacyDirectoryAdoptCurrent,
			testyardmigration.StateLegacyFlatAdoptCurrent,
			testyardmigration.StateCurrent:
		default:
			return fmt.Errorf("typed migration operation has invalid prepared state %q", operation.Before)
		}
	case OperationKindTestYardRouteConsumersV1:
		if err := testyardmigration.ValidateRouteConsumersState(operation.Before); err != nil {
			return fmt.Errorf("typed migration operation has invalid prepared state: %w", err)
		}
	case OperationKindTestVMBrokerRuntimeV1:
		switch testyardmigration.BrokerRuntimeState(operation.Before) {
		case testyardmigration.BrokerRuntimeAbsent,
			testyardmigration.BrokerRuntimeInactive,
			testyardmigration.BrokerRuntimeActive:
		default:
			return fmt.Errorf("typed migration operation has invalid prepared state %q", operation.Before)
		}
	case OperationKindPowerReconcilerRuntimeV1,
		OperationKindPowerReconcilerSystemdCompatV1:
		return validatePowerReconcilerState(operation.Before)
	default:
		return fmt.Errorf("unsupported migration operation kind %q", operation.Kind)
	}
	switch operation.Phase {
	case "", operationPrepared, operationCommitting, operationCommitted,
		operationRollingBack, operationRolledBack:
		return nil
	default:
		return fmt.Errorf("typed migration operation has invalid phase %q", operation.Phase)
	}
}
