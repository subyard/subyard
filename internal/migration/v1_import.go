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
	"syscall"

	"github.com/Subyard/Subyard/internal/releasetransition"
)

// V1ImportRelease identifies one of the only published v1 release journals
// that the V2 transition may import. It is deliberately closed rather than a
// version string supplied by a caller.
type V1ImportRelease string

const (
	V1ImportRelease041 V1ImportRelease = "0.4.1"
	V1ImportRelease042 V1ImportRelease = "0.4.2"
)

// V1ImportCheckpoint identifies a reachable published rollback checkpoint.
// It is input to a fact observer, never an instruction to replay a rollback.
type V1ImportCheckpoint string

const (
	V1ImportCheckpoint041OwnerRollingBack V1ImportCheckpoint = "041-owner-rolling-back"
	V1ImportCheckpoint041OwnerRolledBack  V1ImportCheckpoint = "041-owner-rolled-back"
	V1ImportCheckpoint041Terminal         V1ImportCheckpoint = "041-terminal"

	V1ImportCheckpoint042BrokerRollingBack V1ImportCheckpoint = "042-broker-rolling-back"
	V1ImportCheckpoint042BrokerRolledBack  V1ImportCheckpoint = "042-broker-rolled-back"
	V1ImportCheckpoint042RoutesRollingBack V1ImportCheckpoint = "042-routes-rolling-back"
	V1ImportCheckpoint042RoutesRolledBack  V1ImportCheckpoint = "042-routes-rolled-back"
	V1ImportCheckpoint042OwnerRollingBack  V1ImportCheckpoint = "042-owner-rolling-back"
	V1ImportCheckpoint042OwnerRolledBack   V1ImportCheckpoint = "042-owner-rolled-back"
	V1ImportCheckpoint042Terminal          V1ImportCheckpoint = "042-terminal"
)

// V1ImportRuntimePair is the historical pair recorded by the v1 journal.
// Values are runtime link targets (for example, releases/0.4.1-...).
type V1ImportRuntimePair struct {
	Current  string
	Previous string
}

// V1ImportFactExpectation is the closed factual contract passed to the host
// adapter. The adapter observes and validates facts only; it has no mutation,
// authorization, command, or journal capability.
type V1ImportFactExpectation struct {
	Release     V1ImportRelease
	Checkpoint  V1ImportCheckpoint
	Transaction releasetransition.TransactionID
	RuntimePair V1ImportRuntimePair
}

// V1ImportFactObserver observes the factual owner, route, and broker state
// required by one closed published checkpoint. It returns a normalized factual
// SHA-256 fingerprint after validating that state.
type V1ImportFactObserver interface {
	ObserveV1ImportFacts(context.Context, V1ImportFactExpectation) (releasetransition.Fingerprint, error)
}

// V1ImportFactObserverFunc adapts a function for host-free tests and narrow
// runtime adapters.
type V1ImportFactObserverFunc func(
	context.Context,
	V1ImportFactExpectation,
) (releasetransition.Fingerprint, error)

func (function V1ImportFactObserverFunc) ObserveV1ImportFacts(
	ctx context.Context,
	expectation V1ImportFactExpectation,
) (releasetransition.Fingerprint, error) {
	return function(ctx, expectation)
}

// V1ImportOptions contains only stable read roots and a factual observer. In
// particular it intentionally excludes executables, commands, credentials,
// environment, link writers, and journal writers.
type V1ImportOptions struct {
	ConfigHome  string
	RuntimeRoot string
	Facts       V1ImportFactObserver
}

// V1Import is the bounded result that a closed V2 ingress adapter turns into
// its identity operation. StaticDigest remains valid after target activation;
// FactDigest additionally binds the pre-transition runtime facts and is used
// only until that ingress step is verified.
type V1Import struct {
	Release        V1ImportRelease
	Checkpoint     V1ImportCheckpoint
	Transaction    releasetransition.TransactionID
	RuntimePair    V1ImportRuntimePair
	JournalDigest  releasetransition.Fingerprint
	RegistryDigest releasetransition.Fingerprint
	StaticDigest   releasetransition.Fingerprint
	FactDigest     releasetransition.Fingerprint
}

// V1ImportReader only reads the two retained published v1 inputs. It does not
// lock or write v1 metadata and does not invoke the previous migration engine.
type V1ImportReader struct {
	options V1ImportOptions
}

func NewV1ImportReader(options V1ImportOptions) (*V1ImportReader, error) {
	if !validV1ImportRoot(options.ConfigHome) || !validV1ImportRoot(options.RuntimeRoot) {
		return nil, errors.New("v1 import requires absolute non-root config and runtime roots")
	}
	if options.Facts == nil {
		return nil, errors.New("v1 import requires a factual observer")
	}
	options.ConfigHome = filepath.Clean(options.ConfigHome)
	options.RuntimeRoot = filepath.Clean(options.RuntimeRoot)
	return &V1ImportReader{options: options}, nil
}

// InspectFresh selects a v1 journal only while its exact published pair is
// still live. A normal later update from the candidate therefore has no v1
// import work even though the old journal remains immutable input on disk.
func (reader *V1ImportReader) InspectFresh(
	ctx context.Context,
	pair V1ImportRuntimePair,
) (V1Import, bool, error) {
	if reader == nil {
		return V1Import{}, false, errors.New("v1 import reader is required")
	}
	if !publishedV1CurrentRuntime(pair.Current) {
		return V1Import{}, false, nil
	}
	result, err := reader.inspect(ctx, pair, true)
	if err != nil {
		return V1Import{}, false, err
	}
	return result, true, nil
}

// InspectBound reselects the journal using the pair frozen in a V2 journal.
// It deliberately does not read live links, so resume remains possible after
// V2 has switched the candidate runtime to current.
func (reader *V1ImportReader) InspectBound(
	ctx context.Context,
	pair V1ImportRuntimePair,
) (V1Import, error) {
	if reader == nil {
		return V1Import{}, errors.New("v1 import reader is required")
	}
	if !publishedV1CurrentRuntime(pair.Current) {
		return V1Import{}, errors.New("bound v1 import has an unsupported source runtime")
	}
	return reader.inspect(ctx, pair, true)
}

// InspectBoundStatic reselects only the immutable v1 journal and retained
// registry after V2 has verified the legacy ingress step. It intentionally
// does not re-observe owner, route, or broker facts that target reconciliation
// is allowed to change after activation.
func (reader *V1ImportReader) InspectBoundStatic(
	ctx context.Context,
	pair V1ImportRuntimePair,
) (V1Import, error) {
	if reader == nil {
		return V1Import{}, errors.New("v1 import reader is required")
	}
	if !publishedV1CurrentRuntime(pair.Current) {
		return V1Import{}, errors.New("bound v1 import has an unsupported source runtime")
	}
	return reader.inspect(ctx, pair, false)
}

func (reader *V1ImportReader) inspect(
	ctx context.Context,
	pair V1ImportRuntimePair,
	observeFacts bool,
) (V1Import, error) {
	if pair.Previous != publishedV1PreviousRuntime {
		return V1Import{}, errors.New("v1 import runtime pair is not an exact published pair")
	}
	candidate, err := reader.selectCandidate(pair)
	if err != nil {
		return V1Import{}, err
	}
	registryPayload, registryDigest, err := reader.readRetainedRegistry(candidate.transaction)
	if err != nil {
		return V1Import{}, err
	}
	registry, err := parseV1ImportRegistry(registryPayload)
	if err != nil {
		return V1Import{}, err
	}
	if !knownV1ImportRegistry(candidate.transaction, registry) {
		return V1Import{}, errors.New("retained v1 registry is not the exact published contract")
	}
	if err := validateTransaction(ReleaseOptions{
		ConfigHome: reader.options.ConfigHome, RuntimeRoot: reader.options.RuntimeRoot,
		RegistryPath: candidate.registryPath, Version: candidate.transaction.ToRelease,
	}, registry, candidate.transaction); err != nil {
		return V1Import{}, fmt.Errorf("validate retained v1 registry: %w", err)
	}
	release, checkpoint, valid := v1ImportCheckpoint(candidate.transaction)
	if !valid {
		return V1Import{}, errors.New("v1 import journal has an unsupported published checkpoint")
	}
	transaction := v1CompatibilityTransactionID(candidate.transaction)
	journalDigest := fingerprintV1ImportPayload(candidate.payload)
	staticDigest, err := fingerprintV1ImportValue(struct {
		SchemaVersion  int                             `json:"schemaVersion"`
		Transaction    releasetransition.TransactionID `json:"transaction"`
		RuntimePair    V1ImportRuntimePair             `json:"runtimePair"`
		Checkpoint     V1ImportCheckpoint              `json:"checkpoint"`
		JournalDigest  releasetransition.Fingerprint   `json:"journalDigest"`
		RegistryDigest releasetransition.Fingerprint   `json:"registryDigest"`
	}{
		SchemaVersion: 1, Transaction: transaction, RuntimePair: pair, Checkpoint: checkpoint,
		JournalDigest: journalDigest, RegistryDigest: registryDigest,
	})
	if err != nil {
		return V1Import{}, err
	}
	factDigest := releasetransition.Fingerprint("")
	if observeFacts {
		facts, err := reader.options.Facts.ObserveV1ImportFacts(ctx, V1ImportFactExpectation{
			Release: release, Checkpoint: checkpoint, Transaction: transaction, RuntimePair: pair,
		})
		if err != nil {
			return V1Import{}, fmt.Errorf("observe v1 import facts: %w", err)
		}
		if !validV1ImportFingerprint(facts) {
			return V1Import{}, errors.New("v1 import factual observer returned an invalid fingerprint")
		}
		factDigest, err = fingerprintV1ImportValue(struct {
			SchemaVersion int                           `json:"schemaVersion"`
			StaticDigest  releasetransition.Fingerprint `json:"staticDigest"`
			Facts         releasetransition.Fingerprint `json:"facts"`
		}{SchemaVersion: 1, StaticDigest: staticDigest, Facts: facts})
		if err != nil {
			return V1Import{}, err
		}
	}
	return V1Import{
		Release: release, Checkpoint: checkpoint, Transaction: transaction, RuntimePair: pair,
		JournalDigest: journalDigest, RegistryDigest: registryDigest,
		StaticDigest: staticDigest, FactDigest: factDigest,
	}, nil
}

func (reader *V1ImportReader) selectCandidate(pair V1ImportRuntimePair) (v1CompatibilityCandidate, error) {
	candidates, invalid, current, err := discoverV1CompatibilityJournals(
		reader.options.ConfigHome, "",
	)
	if err != nil {
		return v1CompatibilityCandidate{}, fmt.Errorf("discover v1 import journals: %w", err)
	}
	if invalid != 0 || len(current) != 0 {
		return v1CompatibilityCandidate{}, errors.New("v1 import journal set is invalid or ambiguous")
	}
	if len(candidates) != 1 {
		return v1CompatibilityCandidate{}, errors.New("v1 import requires exactly one published journal")
	}
	candidate := candidates[0]
	if candidate.transaction.ToRuntime != pair.Current ||
		candidate.transaction.FromRuntime != pair.Previous {
		return v1CompatibilityCandidate{}, errors.New("v1 import journal does not match its exact runtime pair")
	}
	if !knownPublishedV1Recovery(candidate.transaction) &&
		!knownPublishedV1Terminal(candidate.transaction) {
		return v1CompatibilityCandidate{}, errors.New("v1 import journal is not a supported published recovery")
	}
	registryPath, err := resolveV1CompatibilityRegistry(reader.options.RuntimeRoot, candidate.transaction)
	if err != nil {
		return v1CompatibilityCandidate{}, fmt.Errorf("resolve retained v1 registry: %w", err)
	}
	candidate.registryPath = registryPath
	return candidate, nil
}

func (reader *V1ImportReader) readRetainedRegistry(
	tx transaction,
) ([]byte, releasetransition.Fingerprint, error) {
	path, err := resolveV1CompatibilityRegistry(reader.options.RuntimeRoot, tx)
	if err != nil {
		return nil, "", err
	}
	payload, err := readV1ImportPublishedFile(path, maximumV1CompatibilityBytes)
	if err != nil {
		return nil, "", fmt.Errorf("read retained v1 registry: %w", err)
	}
	return payload, fingerprintV1ImportPayload(payload), nil
}

func parseV1ImportRegistry(payload []byte) (Registry, error) {
	var registry Registry
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registry); err != nil {
		return Registry{}, fmt.Errorf("decode retained v1 registry: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Registry{}, fmt.Errorf("decode retained v1 registry: %w", err)
	}
	if err := registry.Validate(); err != nil {
		return Registry{}, fmt.Errorf("validate retained v1 registry: %w", err)
	}
	return registry, nil
}

func knownV1ImportRegistry(tx transaction, registry Registry) bool {
	if registry.SchemaVersion != registrySchemaVersion || registry.MinimumLayout != 1 ||
		len(registry.Migrations) == 0 {
		return false
	}
	owner := Definition{
		ID: "migrate-test-yard-owner", FromLayout: 1, ToLayout: 2,
		Resources:      []string{"test-yard-owner", "test-yard-route-consumers"},
		FinalizePolicy: orderedFinalizePolicy, RollbackPolicy: orderedRollbackPolicy,
		Operations: []Operation{
			{ID: "test-yard-owner", Kind: OperationKindTestYardOwnerV1},
			{ID: "test-yard-route-consumers", Kind: OperationKindTestYardRouteConsumersV1},
		},
	}
	if !equalV1ImportDefinition(registry.Migrations[0], owner) {
		return false
	}
	switch tx.ToRelease {
	case "0.4.1":
		return registry.CurrentLayout == 2 && len(registry.Migrations) == 1
	case "0.4.2":
		broker := Definition{
			ID: "refresh-test-vm-broker", FromLayout: 2, ToLayout: 3,
			Resources:      []string{"test-vm-broker-runtime"},
			FinalizePolicy: orderedFinalizePolicy, RollbackPolicy: orderedRollbackPolicy,
			Operations: []Operation{{
				ID: "test-vm-broker-runtime", Kind: OperationKindTestVMBrokerRuntimeV1,
			}},
		}
		return registry.CurrentLayout == 3 && len(registry.Migrations) == 2 &&
			equalV1ImportDefinition(registry.Migrations[1], broker)
	default:
		return false
	}
}

func equalV1ImportDefinition(actual, expected Definition) bool {
	if actual.ID != expected.ID || actual.FromLayout != expected.FromLayout ||
		actual.ToLayout != expected.ToLayout ||
		actual.FinalizePolicy != expected.FinalizePolicy ||
		actual.RollbackPolicy != expected.RollbackPolicy ||
		!slices.Equal(actual.Resources, expected.Resources) || len(actual.Moves) != 0 ||
		len(actual.Operations) != len(expected.Operations) {
		return false
	}
	for index, operation := range actual.Operations {
		if operation != expected.Operations[index] {
			return false
		}
	}
	return true
}

func v1ImportCheckpoint(tx transaction) (V1ImportRelease, V1ImportCheckpoint, bool) {
	if !knownPublishedV1Recovery(tx) && !knownPublishedV1Terminal(tx) {
		return "", "", false
	}
	terminal := tx.Phase == "rolled-back"
	switch tx.ToRelease {
	case "0.4.1":
		if terminal {
			return V1ImportRelease041, V1ImportCheckpoint041Terminal, true
		}
		switch tx.Operations[0].Phase {
		case operationRollingBack:
			return V1ImportRelease041, V1ImportCheckpoint041OwnerRollingBack, true
		case operationRolledBack:
			return V1ImportRelease041, V1ImportCheckpoint041OwnerRolledBack, true
		}
	case "0.4.2":
		if terminal {
			return V1ImportRelease042, V1ImportCheckpoint042Terminal, true
		}
		phases := []string{tx.Operations[0].Phase, tx.Operations[1].Phase, tx.Operations[2].Phase}
		switch {
		case slices.Equal(phases, []string{operationCommitted, operationCommitted, operationRollingBack}):
			return V1ImportRelease042, V1ImportCheckpoint042BrokerRollingBack, true
		case slices.Equal(phases, []string{operationCommitted, operationCommitted, operationRolledBack}):
			return V1ImportRelease042, V1ImportCheckpoint042BrokerRolledBack, true
		case slices.Equal(phases, []string{operationCommitted, operationRollingBack, operationRolledBack}):
			return V1ImportRelease042, V1ImportCheckpoint042RoutesRollingBack, true
		case slices.Equal(phases, []string{operationCommitted, operationRolledBack, operationRolledBack}):
			return V1ImportRelease042, V1ImportCheckpoint042RoutesRolledBack, true
		case slices.Equal(phases, []string{operationRollingBack, operationRolledBack, operationRolledBack}):
			return V1ImportRelease042, V1ImportCheckpoint042OwnerRollingBack, true
		case slices.Equal(phases, []string{operationRolledBack, operationRolledBack, operationRolledBack}):
			return V1ImportRelease042, V1ImportCheckpoint042OwnerRolledBack, true
		}
	}
	return "", "", false
}

func readV1ImportProtectedFile(path string, maximum int64) ([]byte, error) {
	return readV1ImportRegularFile(path, maximum, 0o600)
}

func readV1ImportPublishedFile(path string, maximum int64) ([]byte, error) {
	return readV1ImportRegularFile(path, maximum, 0o644)
}

func readV1ImportRegularFile(path string, maximum int64, expectedMode os.FileMode) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm() != expectedMode || info.Size() <= 0 || info.Size() > maximum {
		return nil, errors.New("v1 import file is not a bounded regular file")
	}
	if err := validateOwnedSafeMode(path, info); err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return nil, errors.New("v1 import file must have exactly one hard link")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	openedStat, ok := opened.Sys().(*syscall.Stat_t)
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() ||
		opened.Mode().Perm() != expectedMode || opened.Size() <= 0 || opened.Size() > maximum ||
		!ok || openedStat.Nlink != 1 {
		return nil, errors.New("v1 import file changed while opening")
	}
	if err := validateOwnedSafeMode(path, opened); err != nil {
		return nil, err
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maximum {
		return nil, errors.New("v1 import file exceeds its size bound")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || after.Size() != int64(len(payload)) ||
		after.Mode() != opened.Mode() {
		return nil, errors.New("v1 import file changed while reading")
	}
	return payload, nil
}

func validV1ImportRoot(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) != string(filepath.Separator)
}

func publishedV1CurrentRuntime(runtime string) bool {
	return runtime == publishedV1041Runtime || runtime == publishedV1042Runtime
}

func fingerprintV1ImportPayload(payload []byte) releasetransition.Fingerprint {
	digest := sha256.Sum256(payload)
	return releasetransition.Fingerprint(hex.EncodeToString(digest[:]))
}

func fingerprintV1ImportValue(value any) (releasetransition.Fingerprint, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return fingerprintV1ImportPayload(payload), nil
}

func validV1ImportFingerprint(value releasetransition.Fingerprint) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}
