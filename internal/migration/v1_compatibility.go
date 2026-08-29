package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

const (
	maximumV1CompatibilityJournals = 64
	maximumV1CompatibilityBytes    = 1 << 20
	publishedV1PreviousRuntime     = "releases/0.4.0-68b9925f6880"
	publishedV1041Runtime          = "releases/0.4.1-fc5b03078508"
	publishedV1042Runtime          = "releases/0.4.2-17608894ab09"
)

type v1CompatibilityCandidate struct {
	transaction  transaction
	id           releasetransition.TransactionID
	payload      []byte
	registryPath string
	terminal     bool
}

type v1RouteConsumersState struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Active        bool                      `json:"active"`
	Consumers     []v1RouteConsumerSnapshot `json:"consumers"`
}

type v1RouteConsumerSnapshot struct {
	Project  string `json:"project"`
	Instance string `json:"instance"`
	Yard     string `json:"yard"`
	Mounted  bool   `json:"mounted"`
}

// InspectMutationGate is the only ordinary-command compatibility surface.
// It classifies protected v1 metadata without locking, writing or resuming it;
// yard update imports the same exact journal through V1ImportReader.
func InspectMutationGate(
	ctx context.Context,
	options ReleaseOptions,
) (*releasetransition.Outcome, error) {
	if !validV1ImportRoot(options.ConfigHome) {
		return nil, errors.New("mutation gate config home must be an absolute non-root path")
	}
	legacy, invalid, current, discoveryErr := discoverV1CompatibilityJournals(
		options.ConfigHome, options.Version,
	)
	if discoveryErr == nil && len(legacy) == 0 && invalid == 0 && len(current) == 0 {
		return nil, nil
	}
	outcome := v1MutationGateOutcome(options)
	operatorAction := func(code releasetransition.OutcomeCode, message string) (*releasetransition.Outcome, error) {
		outcome.Status = releasetransition.StatusOperatorActionRequired
		outcome.Code = code
		outcome.Message = message
		outcome.Retry = "run yard update --check"
		return &outcome, nil
	}
	if discoveryErr != nil {
		return operatorAction(
			releasetransition.CodeJournalInvalid,
			"release transition metadata is unsafe or invalid",
		)
	}
	if len(legacy)+invalid+len(current) > 1 {
		return operatorAction(
			releasetransition.CodeRecoveryAmbiguous,
			"multiple unfinished release transitions require operator review",
		)
	}
	if invalid == 1 {
		return operatorAction(
			releasetransition.CodeJournalInvalid,
			"release transition metadata is foreign or unsupported",
		)
	}
	var candidate v1CompatibilityCandidate
	if len(current) == 1 {
		candidate = current[0]
		registry, err := LoadRegistry(options.RegistryPath)
		if err != nil || validateTransaction(options, registry, candidate.transaction) != nil {
			return operatorAction(
				releasetransition.CodeJournalInvalid,
				"the current release transition is invalid or foreign",
			)
		}
		outcome.Target = releasetransition.ReleaseID(candidate.transaction.ToRelease)
	} else {
		candidate = legacy[0]
		links, err := releasetransition.NewRuntimeLinkStore(options.RuntimeRoot)
		if err != nil {
			return operatorAction(
				releasetransition.CodeRecoveryAmbiguous,
				"runtime release links cannot be safely observed",
			)
		}
		observed, err := links.Observe()
		if err != nil {
			return operatorAction(
				releasetransition.CodeRecoveryAmbiguous,
				"runtime release links cannot be safely observed",
			)
		}
		expectedPrevious := releaseIDFromRuntimeTarget(candidate.transaction.FromRuntime)
		if observed.Active != releaseIDFromRuntimeTarget(candidate.transaction.ToRuntime) ||
			observed.Previous == nil || *observed.Previous != expectedPrevious {
			if err := validatePublishedV1Candidate(
				options.ConfigHome, options.RuntimeRoot, candidate,
			); err != nil {
				return operatorAction(
					releasetransition.CodeJournalInvalid,
					"the retained v1 release transition is invalid or foreign",
				)
			}
			imported, proofErr := completedV2ImportProof(ctx, options, candidate)
			if proofErr != nil {
				return operatorAction(
					releasetransition.CodeJournalInvalid,
					"the v2 import evidence is unsafe or invalid",
				)
			}
			if !imported {
				return operatorAction(
					releasetransition.CodeRecoveryAmbiguous,
					"the retained v1 transition has no completed v2 import evidence",
				)
			}
			return nil, nil
		}
		if err := validatePublishedV1Candidate(
			options.ConfigHome, options.RuntimeRoot, candidate,
		); err != nil {
			return operatorAction(
				releasetransition.CodeJournalInvalid,
				"the retained v1 release transition is invalid or foreign",
			)
		}
	}
	outcome.Status = releasetransition.StatusRecovering
	outcome.Code = releasetransition.CodeRecoveryPending
	outcome.Message = "an unfinished release transition is waiting for yard update"
	outcome.Retry = "run yard update"
	outcome.Transaction = &candidate.id
	return &outcome, nil
}

func completedV2ImportProof(
	ctx context.Context,
	options ReleaseOptions,
	candidate v1CompatibilityCandidate,
) (bool, error) {
	pair := V1ImportRuntimePair{
		Current: candidate.transaction.ToRuntime, Previous: candidate.transaction.FromRuntime,
	}
	reader, err := NewV1ImportReader(V1ImportOptions{
		ConfigHome: options.ConfigHome, RuntimeRoot: options.RuntimeRoot,
		Facts: V1ImportFactObserverFunc(func(
			context.Context,
			V1ImportFactExpectation,
		) (releasetransition.Fingerprint, error) {
			return "", errors.New("v1 import facts are unavailable after activation")
		}),
	})
	if err != nil {
		return false, err
	}
	imported, err := reader.InspectBoundStatic(ctx, pair)
	if err != nil {
		return false, err
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return false, err
	}
	from := releaseIDFromRuntimeTarget(candidate.transaction.ToRuntime)
	previous := releaseIDFromRuntimeTarget(candidate.transaction.FromRuntime)
	receipt, err := store.ReadCompatibilityEvidence(imported.StaticDigest)
	if err != nil || !receipt.Exists {
		return false, err
	}
	evidence, err := releasetransition.ParseCompatibilityEvidence(receipt.Payload)
	if err != nil {
		return false, err
	}
	return evidence.Kind == releasetransition.V2LegacyV1Import &&
		evidence.From == from && evidence.Previous == previous &&
		evidence.Identity == imported.StaticDigest, nil
}

func v1MutationGateOutcome(options ReleaseOptions) releasetransition.Outcome {
	current := currentRuntimeTarget(options.RuntimeRoot)
	previous := runtimeLinkTarget(options.RuntimeRoot, "previous")
	return releasetransition.Outcome{
		Active:   releaseIDFromRuntimeTarget(current),
		Previous: releaseIDPointerFromRuntimeTarget(previous),
		Target:   releasetransition.ReleaseID(options.Version),
	}
}

func validatePublishedV1Candidate(
	configHome string,
	runtimeRoot string,
	candidate v1CompatibilityCandidate,
) error {
	registryPath, err := resolveV1CompatibilityRegistry(runtimeRoot, candidate.transaction)
	if err != nil {
		return err
	}
	payload, err := readV1ImportPublishedFile(registryPath, maximumV1CompatibilityBytes)
	if err != nil {
		return err
	}
	registry, err := parseV1ImportRegistry(payload)
	if err != nil || !knownV1ImportRegistry(candidate.transaction, registry) {
		return errors.New("retained v1 registry is not the exact published contract")
	}
	return validateTransaction(ReleaseOptions{
		ConfigHome: configHome, RuntimeRoot: runtimeRoot,
		RegistryPath: registryPath, Version: candidate.transaction.ToRelease,
	}, registry, candidate.transaction)
}

func discoverV1CompatibilityJournals(
	configHome string,
	currentVersion string,
) ([]v1CompatibilityCandidate, int, []v1CompatibilityCandidate, error) {
	root := migrationRoot(configHome)
	transactionsRoot := filepath.Join(root, "transactions")
	if err := validateV1CompatibilityDirectory(root); errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil, nil
	} else if err != nil {
		return nil, 0, nil, err
	}
	if err := validateV1CompatibilityDirectory(transactionsRoot); errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil, nil
	} else if err != nil {
		return nil, 0, nil, err
	}
	directory, err := os.OpenFile(
		transactionsRoot, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0,
	)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("open v1 compatibility transactions: %w", err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(maximumV1CompatibilityJournals + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, nil, fmt.Errorf("read v1 compatibility transactions: %w", err)
	}
	if len(entries) > maximumV1CompatibilityJournals {
		return nil, 0, nil, errors.New("too many v1 migration journal directories")
	}
	var legacy, current []v1CompatibilityCandidate
	invalid := 0
	for _, entry := range entries {
		if !safeRelativePath(entry.Name()) {
			return nil, 0, nil, errors.New("v1 migration journal directory has an unsafe name")
		}
		journalPath := filepath.Join(transactionsRoot, entry.Name(), "transaction.json")
		if err := validateV1CompatibilityDirectory(filepath.Dir(journalPath)); err != nil {
			return nil, 0, nil, err
		}
		tx, payload, exists, err := readV1CompatibilityJournal(journalPath)
		if err != nil {
			return nil, 0, nil, err
		}
		if !exists {
			invalid++
			continue
		}
		candidate := v1CompatibilityCandidate{
			transaction: tx, id: v1CompatibilityTransactionID(tx),
			payload: slices.Clone(payload),
		}
		canonicalPath := filepath.Clean(transactionPath(configHome, tx.ToRelease))
		if tx.ToRelease == currentVersion && filepath.Clean(journalPath) == canonicalPath {
			if tx.Phase != "committed" && tx.Phase != "rolled-back" {
				current = append(current, candidate)
			}
			continue
		}
		if tx.Phase == "committed" {
			continue
		}
		candidate.terminal = tx.Phase == "rolled-back"
		if filepath.Clean(journalPath) != canonicalPath ||
			candidate.terminal && !knownPublishedV1Terminal(tx) ||
			!candidate.terminal && !knownPublishedV1Recovery(tx) {
			invalid++
			continue
		}
		legacy = append(legacy, candidate)
	}
	return legacy, invalid, current, nil
}

func knownPublishedV1Terminal(tx transaction) bool {
	if tx.Phase != "rolled-back" {
		return false
	}
	for _, operation := range tx.Operations {
		if operation.Phase != operationRolledBack {
			return false
		}
	}
	tx.Phase = "rolling-back"
	return knownPublishedV1Recovery(tx)
}

func validateV1CompatibilityDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("v1 migration journal directory is not protected")
	}
	return validateOwnedSafeMode(path, info)
}

func readV1CompatibilityJournal(path string) (transaction, []byte, bool, error) {
	payload, err := readV1ImportProtectedFile(path, maximumV1CompatibilityBytes)
	if errors.Is(err, os.ErrNotExist) {
		return transaction{}, nil, false, nil
	}
	if err != nil {
		return transaction{}, nil, false, err
	}
	var tx transaction
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tx); err != nil {
		return transaction{}, nil, false, fmt.Errorf("decode v1 migration journal: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return transaction{}, nil, false, err
	}
	return tx, payload, true, nil
}

func knownPublishedV1Recovery(tx transaction) bool {
	if tx.SchemaVersion != 1 || tx.FromLayout != 1 ||
		tx.FromRuntime != publishedV1PreviousRuntime || tx.ToRuntime == "" ||
		tx.Phase != "rolling-back" || !tx.RollbackOps || len(tx.Entries) != 0 ||
		!safeReleaseIdentity(tx.FromRuntime) || !safeReleaseIdentity(tx.ToRuntime) ||
		!knownV1RouteConsumersBefore(tx.Operations) {
		return false
	}
	switch tx.ToRelease {
	case "0.4.1":
		return tx.ToRuntime == publishedV1041Runtime && tx.ToLayout == 2 &&
			slices.Equal(tx.Migrations, []string{"migrate-test-yard-owner"}) &&
			len(tx.Operations) == 2 &&
			knownV1Operation(tx.Operations[0], "migrate-test-yard-owner", "test-yard-owner",
				OperationKindTestYardOwnerV1, "current", tx.Operations[0].Phase) &&
			(tx.Operations[0].Phase == operationRollingBack ||
				tx.Operations[0].Phase == operationRolledBack) &&
			tx.Operations[1].Phase == operationRolledBack
	case "0.4.2":
		return tx.ToRuntime == publishedV1042Runtime && tx.ToLayout == 3 &&
			slices.Equal(tx.Migrations, []string{
				"migrate-test-yard-owner", "refresh-test-vm-broker",
			}) && len(tx.Operations) == 3 &&
			knownV1Operation(tx.Operations[0], "migrate-test-yard-owner", "test-yard-owner",
				OperationKindTestYardOwnerV1, "current", tx.Operations[0].Phase) &&
			knownV1Operation(tx.Operations[2], "refresh-test-vm-broker", "test-vm-broker-runtime",
				OperationKindTestVMBrokerRuntimeV1, "active", tx.Operations[2].Phase) &&
			knownPublished042RollbackProgress(tx.Operations)
	default:
		return false
	}
}

func knownPublished042RollbackProgress(operations []transactionOperation) bool {
	owner, routes, broker := operations[0].Phase, operations[1].Phase, operations[2].Phase
	return owner == operationCommitted && routes == operationCommitted &&
		(broker == operationRollingBack || broker == operationRolledBack) ||
		owner == operationCommitted &&
			(routes == operationRollingBack || routes == operationRolledBack) &&
			broker == operationRolledBack ||
		(owner == operationRollingBack || owner == operationRolledBack) &&
			routes == operationRolledBack && broker == operationRolledBack
}

func knownV1RouteConsumersBefore(operations []transactionOperation) bool {
	if len(operations) < 2 || !knownV1Operation(
		operations[1], "migrate-test-yard-owner", "test-yard-route-consumers",
		OperationKindTestYardRouteConsumersV1, operations[1].Before, operations[1].Phase,
	) {
		return false
	}
	var state v1RouteConsumersState
	decoder := json.NewDecoder(strings.NewReader(operations[1].Before))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&state) == nil && requireJSONEOF(decoder) == nil &&
		state.SchemaVersion == 1 && state.Active &&
		slices.Equal(state.Consumers, []v1RouteConsumerSnapshot{{
			Project: "subyard", Instance: "yard", Yard: "default", Mounted: false,
		}})
}

func knownV1Operation(
	operation transactionOperation,
	migrationID, operationID, kind, before, phase string,
) bool {
	return operation.MigrationID == migrationID && operation.OperationID == operationID &&
		operation.Kind == kind && operation.Before == before && operation.Phase == phase
}

func v1CompatibilityTransactionID(tx transaction) releasetransition.TransactionID {
	type operationIdentity struct {
		MigrationID string `json:"migrationId"`
		OperationID string `json:"operationId"`
		Kind        string `json:"kind"`
		Before      string `json:"before"`
	}
	identity := struct {
		FromLayout  int                 `json:"fromLayout"`
		ToLayout    int                 `json:"toLayout"`
		FromRuntime string              `json:"fromRuntime"`
		ToRelease   string              `json:"toRelease"`
		ToRuntime   string              `json:"toRuntime"`
		Migrations  []string            `json:"migrations"`
		Operations  []operationIdentity `json:"operations"`
	}{
		tx.FromLayout, tx.ToLayout, tx.FromRuntime, tx.ToRelease, tx.ToRuntime,
		tx.Migrations, make([]operationIdentity, 0, len(tx.Operations)),
	}
	for _, operation := range tx.Operations {
		identity.Operations = append(identity.Operations, operationIdentity{
			operation.MigrationID, operation.OperationID, operation.Kind, operation.Before,
		})
	}
	payload, _ := json.Marshal(identity)
	digest := sha256.Sum256(payload)
	return releasetransition.TransactionID("v1-" + hex.EncodeToString(digest[:16]))
}

func resolveV1CompatibilityRegistry(runtimeRoot string, tx transaction) (string, error) {
	if !validV1ImportRoot(runtimeRoot) || tx.ToRuntime == "" || !safeReleaseIdentity(tx.ToRuntime) {
		return "", errors.New("v1 compatibility runtime identity is invalid")
	}
	root := filepath.Clean(runtimeRoot)
	releaseRoot := filepath.Join(root, tx.ToRuntime)
	relative, err := filepath.Rel(filepath.Join(root, "releases"), releaseRoot)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		strings.Contains(relative, string(filepath.Separator)) {
		return "", errors.New("retained v1 runtime escapes the release store")
	}
	if err := validateDirectoryUnder(root, filepath.Join(releaseRoot, "config")); err != nil {
		return "", err
	}
	return filepath.Join(releaseRoot, "config", "migrations.json"), nil
}

func v1CompatibilityRecoveryOptions(
	options ReleaseOptions,
	candidate v1CompatibilityCandidate,
) ReleaseOptions {
	options.Version = candidate.transaction.ToRelease
	options.RegistryPath = candidate.registryPath
	return options
}

func verifyV1CompatibilityCheckpoint(
	ctx context.Context,
	options ReleaseOptions,
	registry Registry,
	tx transaction,
) error {
	path, err := transactionOperationDefinitionsForTransaction(registry, tx)
	if err != nil {
		return err
	}
	for _, definition := range path {
		for _, expected := range definition.Operations {
			index := transactionOperationIndex(tx, definition.ID, expected.ID)
			if index < 0 {
				return fmt.Errorf("migration %q operation %q is missing", definition.ID, expected.ID)
			}
			operation := tx.Operations[index]
			verifyRollback := func() error {
				return verifyTypedOperationRollback(
					ctx, options, expected, operation.Before, tx.FromLayout,
				)
			}
			switch operation.Phase {
			case operationRolledBack:
				err = verifyRollback()
			case operationCommitted:
				err = verifyTypedOperation(ctx, options, expected, operation.Before)
			case operationRollingBack:
				err = verifyRollback()
				if err != nil && expected.Kind == OperationKindTestVMBrokerRuntimeV1 {
					registryPath, resolveErr := resolveV1CompatibilityRegistry(
						options.RuntimeRoot, tx,
					)
					if resolveErr != nil {
						return resolveErr
					}
					retained := options
					retained.RepositoryRoot = filepath.Dir(filepath.Dir(registryPath))
					err = verifyTypedOperation(ctx, retained, expected, operation.Before)
				}
				if err != nil && expected.Kind != OperationKindTestVMBrokerRuntimeV1 {
					err = verifyTypedOperation(ctx, options, expected, operation.Before)
				}
			default:
				err = fmt.Errorf("unsupported checkpoint %q", operation.Phase)
			}
			if err != nil {
				return fmt.Errorf("verify migration %q operation %q: %w", definition.ID, expected.ID, err)
			}
		}
	}
	return nil
}

func releaseIDFromRuntimeTarget(target string) releasetransition.ReleaseID {
	return releasetransition.ReleaseID(strings.TrimPrefix(target, "releases/"))
}

func releaseIDPointerFromRuntimeTarget(target string) *releasetransition.ReleaseID {
	if target == "" {
		return nil
	}
	value := releaseIDFromRuntimeTarget(target)
	return &value
}
