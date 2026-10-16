package migration

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/testyardmigration"
)

const (
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
