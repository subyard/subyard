package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

// ObserveOriginalV1Checkpoint observes the physical operations in original,
// already selected records. Selection, published-contract eligibility and the
// static import digest belong to the Rust owner; this function never discovers
// a journal, reads a retained registry, or resumes an operation.
func ObserveOriginalV1Checkpoint(ctx context.Context, options ReleaseOptions, expectation V1ImportFactExpectation, journalRaw, registryRaw []byte) (releasetransition.Fingerprint, error) {
	if !validV1ImportRoot(options.ConfigHome) || !validV1ImportRoot(options.RuntimeRoot) ||
		len(journalRaw) == 0 || len(journalRaw) > maximumV1CompatibilityBytes ||
		len(registryRaw) == 0 || len(registryRaw) > maximumV1CompatibilityBytes {
		return "", errors.New("original v1 checkpoint inputs are invalid")
	}
	var tx transaction
	decoder := json.NewDecoder(bytes.NewReader(journalRaw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tx); err != nil {
		return "", err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return "", err
	}
	registry, err := parseV1ImportRegistry(registryRaw)
	if err != nil {
		return "", err
	}
	if string(expectation.Release) != tx.ToRelease || expectation.Checkpoint == "" ||
		expectation.Transaction != v1CompatibilityTransactionID(tx) ||
		expectation.RuntimePair.Current != tx.ToRuntime || expectation.RuntimePair.Previous != tx.FromRuntime ||
		!safeReleaseIdentity(expectation.RuntimePair.Current) || !safeReleaseIdentity(expectation.RuntimePair.Previous) {
		return "", errors.New("original v1 checkpoint identity differs")
	}
	// The caller supplies the predicate; the native adapter only accepts the
	// finite operation bodies it can observe, in exactly the supplied order.
	index := 0
	for _, definition := range registry.Migrations {
		for _, expected := range definition.Operations {
			if index >= len(tx.Operations) {
				return "", errors.New("original v1 operation is missing")
			}
			operation := tx.Operations[index]
			index++
			if operation.MigrationID != definition.ID || operation.OperationID != expected.ID || operation.Kind != expected.Kind {
				return "", errors.New("original v1 operation identity differs")
			}
			switch expected.Kind {
			case OperationKindTestYardOwnerV1, OperationKindTestYardRouteConsumersV1, OperationKindTestVMBrokerRuntimeV1:
			default:
				return "", errors.New("original v1 operation has no physical reader")
			}
			rollback := func() error {
				if expected.Kind != OperationKindTestVMBrokerRuntimeV1 {
					return verifyTypedOperationRollback(ctx, options, expected, operation.Before, tx.FromLayout)
				}
				if err := validateBrokerRollbackLayout(tx.FromLayout); err != nil {
					return err
				}
				previous, err := originalV1RuntimeOptions(options, expectation.RuntimePair.Previous)
				if err != nil {
					return err
				}
				return testyardmigration.VerifyBrokerRuntime(ctx, testYardOptions(previous), testyardmigration.BrokerRuntimeState(operation.Before))
			}
			switch operation.Phase {
			case operationRolledBack:
				err = rollback()
			case operationCommitted:
				err = verifyTypedOperation(ctx, options, expected, operation.Before)
			case operationRollingBack:
				err = rollback()
				if err != nil {
					retained := options
					if expected.Kind == OperationKindTestVMBrokerRuntimeV1 {
						retained.RepositoryRoot = filepath.Join(options.RuntimeRoot, expectation.RuntimePair.Current)
					}
					err = verifyTypedOperation(ctx, retained, expected, operation.Before)
				}
			default:
				return "", errors.New("original v1 physical checkpoint is unsupported")
			}
			if err != nil {
				return "", fmt.Errorf("observe original v1 operation: %w", err)
			}
		}
	}
	if index == 0 || index != len(tx.Operations) {
		return "", errors.New("original v1 operation set differs")
	}
	return fingerprintV1ImportValue(struct {
		SchemaVersion int                     `json:"schemaVersion"`
		Expectation   V1ImportFactExpectation `json:"expectation"`
		Class         string                  `json:"class"`
	}{1, expectation, "supported-published-checkpoint"})
}

func originalV1RuntimeOptions(options ReleaseOptions, runtime string) (ReleaseOptions, error) {
	root := filepath.Join(options.RuntimeRoot, runtime)
	if err := validateDirectoryUnder(options.RuntimeRoot, root); err != nil {
		return ReleaseOptions{}, err
	}
	result := options
	result.RepositoryRoot = root
	result.Executable = filepath.Join(root, "bin", "yard-engine")
	info, err := os.Lstat(result.Executable)
	if err != nil {
		return ReleaseOptions{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ReleaseOptions{}, errors.New("original v1 runtime executable is unsafe")
	}
	result.Environment = append([]string(nil), options.Environment...)
	result.Environment = removePowerReconcilerEnvironment(result.Environment, "SUBYARD_POWER_MIGRATION_PAYLOAD_ROOT")
	result.Environment = replacePowerReconcilerEnvironment(result.Environment, "SUBYARD_REPOSITORY_ROOT", root)
	result.Environment = replacePowerReconcilerEnvironment(result.Environment, "SUBYARD_CONFIG_DIR", filepath.Join(root, "config"))
	return result, nil
}
