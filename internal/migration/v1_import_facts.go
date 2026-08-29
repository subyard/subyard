package migration

import (
	"context"
	"errors"
	"fmt"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

type v1ImportFactObserver struct {
	options ReleaseOptions
}

// NewV1ImportFactObserver constructs the read-only host adapter used by the
// bounded importer. It verifies that the historical checkpoint still belongs
// to the supported factual equivalence class; it never advances or rewrites
// the v1 transaction.
func NewV1ImportFactObserver(options ReleaseOptions) (V1ImportFactObserver, error) {
	if !validV1ImportRoot(options.ConfigHome) || !validV1ImportRoot(options.RuntimeRoot) {
		return nil, errors.New("v1 import facts require absolute non-root config and runtime roots")
	}
	return &v1ImportFactObserver{options: options}, nil
}

func (observer *v1ImportFactObserver) ObserveV1ImportFacts(
	ctx context.Context,
	expectation V1ImportFactExpectation,
) (releasetransition.Fingerprint, error) {
	if observer == nil {
		return "", errors.New("v1 import fact observer is required")
	}
	candidates, invalid, current, err := discoverV1CompatibilityJournals(
		observer.options.ConfigHome, "",
	)
	if err != nil || invalid != 0 || len(current) != 0 || len(candidates) != 1 {
		return "", errors.New("v1 import fact journal is invalid or ambiguous")
	}
	candidate := candidates[0]
	pair := V1ImportRuntimePair{
		Current: candidate.transaction.ToRuntime, Previous: candidate.transaction.FromRuntime,
	}
	release, checkpoint, valid := v1ImportCheckpoint(candidate.transaction)
	if !valid || release != expectation.Release || checkpoint != expectation.Checkpoint ||
		v1CompatibilityTransactionID(candidate.transaction) != expectation.Transaction ||
		pair != expectation.RuntimePair {
		return "", errors.New("v1 import fact expectation does not match the published journal")
	}
	registryPath, err := resolveV1CompatibilityRegistry(
		observer.options.RuntimeRoot, candidate.transaction,
	)
	if err != nil {
		return "", err
	}
	candidate.registryPath = registryPath
	payload, err := readV1ImportPublishedFile(registryPath, maximumV1CompatibilityBytes)
	if err != nil {
		return "", errors.New("v1 import facts have no exact retained registry")
	}
	registry, err := parseV1ImportRegistry(payload)
	if err != nil || !knownV1ImportRegistry(candidate.transaction, registry) {
		return "", errors.New("v1 import facts have no exact retained registry")
	}
	if err := verifyV1CompatibilityCheckpoint(
		ctx, v1CompatibilityRecoveryOptions(observer.options, candidate),
		registry, candidate.transaction,
	); err != nil {
		return "", fmt.Errorf("verify v1 import checkpoint facts: %w", err)
	}
	return fingerprintV1ImportValue(struct {
		SchemaVersion int                     `json:"schemaVersion"`
		Expectation   V1ImportFactExpectation `json:"expectation"`
		Class         string                  `json:"class"`
	}{
		SchemaVersion: 1, Expectation: expectation,
		Class: "supported-published-checkpoint",
	})
}
