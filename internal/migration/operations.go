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

func prepareTypedOperation(
	ctx context.Context,
	options ReleaseOptions,
	operation Operation,
) (string, error) {
	switch operation.Kind {
	case OperationKindTestYardOwnerV1:
		state, err := testyardmigration.Prepare(ctx, testYardOptions(options))
		return string(state), err
	case OperationKindTestYardRouteConsumersV1:
		return testyardmigration.PrepareRouteConsumers(ctx, testYardOptions(options))
	case OperationKindTestVMBrokerRuntimeV1:
		state, err := testyardmigration.PrepareBrokerRuntime(ctx, testYardOptions(options))
		return string(state), err
	case OperationKindPowerReconcilerRuntimeV1,
		OperationKindPowerReconcilerSystemdCompatV1:
		return preparePowerReconciler(options)
	default:
		return "", fmt.Errorf("unsupported migration operation kind %q", operation.Kind)
	}
}

func commitTypedOperation(
	ctx context.Context,
	options ReleaseOptions,
	operation Operation,
	before string,
) error {
	switch operation.Kind {
	case OperationKindTestYardOwnerV1:
		state := testyardmigration.State(before)
		if err := testyardmigration.Commit(ctx, testYardOptions(options), state); err != nil {
			return err
		}
		return testyardmigration.Verify(testYardOptions(options), state)
	case OperationKindTestYardRouteConsumersV1:
		return testyardmigration.CommitRouteConsumers(
			ctx,
			testYardOptions(options),
			before,
		)
	case OperationKindTestVMBrokerRuntimeV1:
		return testyardmigration.CommitBrokerRuntime(
			ctx,
			testYardOptions(options),
			testyardmigration.BrokerRuntimeState(before),
		)
	case OperationKindPowerReconcilerRuntimeV1,
		OperationKindPowerReconcilerSystemdCompatV1:
		return commitPowerReconciler(ctx, options, before)
	default:
		return fmt.Errorf("unsupported migration operation kind %q", operation.Kind)
	}
}

func verifyTypedOperation(
	ctx context.Context,
	options ReleaseOptions,
	operation Operation,
	before string,
) error {
	switch operation.Kind {
	case OperationKindTestYardOwnerV1:
		return testyardmigration.Verify(
			testYardOptions(options),
			testyardmigration.State(before),
		)
	case OperationKindTestYardRouteConsumersV1:
		return testyardmigration.VerifyRouteConsumers(
			ctx,
			testYardOptions(options),
			before,
		)
	case OperationKindTestVMBrokerRuntimeV1:
		return testyardmigration.VerifyBrokerRuntime(
			ctx,
			testYardOptions(options),
			testyardmigration.BrokerRuntimeState(before),
		)
	case OperationKindPowerReconcilerRuntimeV1,
		OperationKindPowerReconcilerSystemdCompatV1:
		return verifyPowerReconciler(ctx, options, before)
	default:
		return fmt.Errorf("unsupported migration operation kind %q", operation.Kind)
	}
}

func rollbackTypedOperation(
	ctx context.Context,
	options ReleaseOptions,
	operation Operation,
	before string,
	fromLayout int,
) error {
	return rollbackTypedOperationWithLegacyPower(
		ctx,
		options,
		operation,
		before,
		fromLayout,
		"",
	)
}

func rollbackTypedOperationWithLegacyPower(
	ctx context.Context,
	options ReleaseOptions,
	operation Operation,
	before string,
	fromLayout int,
	legacyDesiredPower string,
) error {
	switch operation.Kind {
	case OperationKindTestYardOwnerV1:
		state := testyardmigration.State(before)
		current := testYardOptions(options)
		legacy := current
		if fromLayout == 1 && state != testyardmigration.StateAbsent &&
			state != testyardmigration.StateCurrent {
			previous, err := previousRuntimeOptions(options)
			if err != nil {
				return err
			}
			legacy = testYardOptions(previous)
		}
		if err := testyardmigration.RollbackWithLegacyRuntimeAndPower(
			ctx,
			current,
			legacy,
			state,
			legacyDesiredPower,
		); err != nil {
			return err
		}
		return testyardmigration.VerifyRollback(testYardOptions(options), state)
	case OperationKindTestYardRouteConsumersV1:
		if err := testyardmigration.RollbackRouteConsumers(
			ctx,
			testYardOptions(options),
			before,
		); err != nil {
			return err
		}
		return testyardmigration.VerifyRouteConsumersRollback(
			ctx,
			testYardOptions(options),
			before,
		)
	case OperationKindTestVMBrokerRuntimeV1:
		disposition, err := brokerRollbackPolicy(fromLayout)
		if err != nil {
			return fmt.Errorf("operation %q: %w", operation.ID, err)
		}
		if disposition == brokerRollbackDeferredToOwner {
			return nil
		}
		return testyardmigration.RollbackBrokerRuntime(
			ctx,
			testYardOptions(options),
			testyardmigration.BrokerRuntimeState(before),
		)
	case OperationKindPowerReconcilerRuntimeV1:
		return rollbackPowerReconciler(ctx, options, before, false, false)
	case OperationKindPowerReconcilerSystemdCompatV1:
		switch fromLayout {
		case 1, 2, 3:
			return rollbackPowerReconciler(ctx, options, before, false, false)
		case 4:
			return rollbackPowerReconciler(ctx, options, before, true, true)
		default:
			return fmt.Errorf(
				"unsupported compatibility layout %d for operation %q",
				fromLayout,
				operation.ID,
			)
		}
	default:
		return fmt.Errorf("unsupported migration operation kind %q", operation.Kind)
	}
}

type brokerRollbackDisposition uint8

const (
	brokerRollbackDeferredToOwner brokerRollbackDisposition = iota
	brokerRollbackWithPrevious
)

func brokerRollbackPolicy(fromLayout int) (brokerRollbackDisposition, error) {
	if fromLayout < 1 {
		return 0, fmt.Errorf("unsupported broker rollback layout %d", fromLayout)
	}
	if fromLayout == 1 {
		return brokerRollbackDeferredToOwner, nil
	}
	return brokerRollbackWithPrevious, nil
}

func reprepareTypedOperation(
	ctx context.Context,
	options ReleaseOptions,
	operation Operation,
	before string,
) (string, bool, error) {
	if err := verifyTypedOperation(ctx, options, operation, before); err == nil {
		return before, false, nil
	}
	switch operation.Kind {
	case OperationKindTestYardOwnerV1:
		previous := testyardmigration.State(before)
		current, err := testyardmigration.Prepare(ctx, testYardOptions(options))
		if err != nil {
			return "", false, err
		}
		switch current {
		case testyardmigration.StateAbsent:
			return string(current), false, nil
		case testyardmigration.StateLegacyDirectory,
			testyardmigration.StateLegacyDirectoryProjects,
			testyardmigration.StateLegacyDirectoryOverrides,
			testyardmigration.StateLegacyDirectoryState,
			testyardmigration.StateLegacyFlat,
			testyardmigration.StateLegacyDirectoryAdoptCurrent,
			testyardmigration.StateLegacyFlatAdoptCurrent,
			testyardmigration.StateCurrent:
			if current == testyardmigration.StateCurrent {
				return "", false, fmt.Errorf(
					"typed migration postcondition changed after commit from %q",
					before,
				)
			}
			if previous != testyardmigration.StateAbsent &&
				previous != testyardmigration.StateCurrent &&
				current != previous {
				return "", false, fmt.Errorf(
					"typed migration postcondition changed from %q to %q",
					before,
					current,
				)
			}
			return string(current), true, nil
		default:
			return "", false, fmt.Errorf(
				"typed migration postcondition changed from %q to %q",
				before,
				current,
			)
		}
	case OperationKindTestYardRouteConsumersV1:
		current, err := testyardmigration.PrepareRouteConsumers(
			ctx,
			testYardOptions(options),
		)
		if err != nil {
			return "", false, err
		}
		return current, true, nil
	case OperationKindTestVMBrokerRuntimeV1:
		current, err := testyardmigration.PrepareBrokerRuntime(
			ctx,
			testYardOptions(options),
		)
		if err != nil {
			return "", false, err
		}
		return string(current), true, nil
	case OperationKindPowerReconcilerRuntimeV1,
		OperationKindPowerReconcilerSystemdCompatV1:
		current, err := preparePowerReconciler(options)
		if err != nil {
			return "", false, err
		}
		return current, current == powerReconcilerInstalled, nil
	default:
		return "", false, fmt.Errorf("unsupported migration operation kind %q", operation.Kind)
	}
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
		Executable:     executable,
		RepositoryRoot: options.RepositoryRoot,
		RuntimeRoot:    options.RuntimeRoot,
		Incus:          options.Incus,
		ConfigHome:     options.ConfigHome,
		DataHome:       options.DataHome,
		Environment:    environment,
		Stdout:         diagnostics,
		Stderr:         stderr,
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
			return fmt.Errorf(
				"typed migration operation has invalid prepared state %q",
				operation.Before,
			)
		}
	case OperationKindTestYardRouteConsumersV1:
		if err := testyardmigration.ValidateRouteConsumersState(operation.Before); err != nil {
			return fmt.Errorf(
				"typed migration operation has invalid prepared state: %w",
				err,
			)
		}
	case OperationKindTestVMBrokerRuntimeV1:
		if err := testyardmigration.ValidateBrokerRuntimeState(
			testyardmigration.BrokerRuntimeState(operation.Before),
		); err != nil {
			return fmt.Errorf(
				"typed migration operation has invalid prepared state: %w",
				err,
			)
		}
	case OperationKindPowerReconcilerRuntimeV1,
		OperationKindPowerReconcilerSystemdCompatV1:
		if err := validatePowerReconcilerState(operation.Before); err != nil {
			return fmt.Errorf(
				"typed migration operation has invalid prepared state: %w",
				err,
			)
		}
	default:
		return fmt.Errorf("unsupported migration operation kind %q", operation.Kind)
	}
	switch operation.Phase {
	case operationPrepared, operationCommitting, operationCommitted,
		operationRollingBack, operationRolledBack:
		return nil
	default:
		return fmt.Errorf("typed migration operation has unknown phase %q", operation.Phase)
	}
}
