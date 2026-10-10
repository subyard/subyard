package releasetransition

import (
	"errors"
	"slices"
)

const migrationLedgerCheckpointName = "history-checkpoint.json"

func (store *POSIXV2Store) ReadMigrationLedger(
	registry RegistryV2,
	registryDigest Fingerprint,
) (MigrationLedgerSnapshot, error) {
	if store == nil {
		return MigrationLedgerSnapshot{}, errors.New("release transition store is required")
	}
	checkpointSnapshot, err := store.ReadMigrationCheckpoint()
	if err != nil {
		return MigrationLedgerSnapshot{}, err
	}
	projection, err := store.ReadLedger()
	if err != nil {
		return MigrationLedgerSnapshot{}, err
	}
	return ParseMigrationLedger(projection, checkpointSnapshot, registry, registryDigest)
}

func (store *POSIXV2Store) ReadMigrationCheckpoint() (ProtectedSnapshot, error) {
	if store == nil {
		return ProtectedSnapshot{}, errors.New("release transition store is required")
	}
	return store.readRecord([]string{"release-transition", "v2"}, migrationLedgerCheckpointName)
}

func ParseMigrationLedger(
	projection ProtectedSnapshot,
	checkpointSnapshot ProtectedSnapshot,
	registry RegistryV2,
	registryDigest Fingerprint,
) (MigrationLedgerSnapshot, error) {
	if !validProtectedSnapshot(projection) || !validProtectedSnapshot(checkpointSnapshot) {
		return MigrationLedgerSnapshot{}, invalid("migration ledger snapshots are malformed")
	}
	if checkpointSnapshot.Exists {
		checkpoint, _, err := ParseLedgerCheckpointV1(
			checkpointSnapshot.Payload, registry, registryDigest, projection,
		)
		if err != nil {
			return MigrationLedgerSnapshot{}, err
		}
		ledger, err := checkpoint.LedgerView(registry)
		if err != nil {
			return MigrationLedgerSnapshot{}, err
		}
		return MigrationLedgerSnapshot{
			Ledger: ledger, Checkpoint: checkpoint,
			AuthoritySnapshot: checkpointSnapshot, ProjectionSnapshot: projection,
			Checkpointed: true,
		}, nil
	}
	ledger := BaselineLedgerV2(registry)
	if projection.Exists {
		var err error
		ledger, _, err = ParseLedgerV2(projection.Payload, registry)
		if err != nil {
			return MigrationLedgerSnapshot{}, err
		}
	}
	return MigrationLedgerSnapshot{
		Ledger: ledger, AuthoritySnapshot: projection,
		ProjectionSnapshot: projection,
	}, nil
}

// CompareAndSwapMigrationLedger mutates only the active authoritative record.
// After checkpoint publication, ledger.json is a read-only compatibility
// projection and its pinned fingerprint is checked on every read.
func (store *POSIXV2Store) CompareAndSwapMigrationLedger(
	expected MigrationLedgerSnapshot,
	next LedgerV2,
	registry RegistryV2,
	registryDigest Fingerprint,
) error {
	if store == nil {
		return errors.New("release transition store is required")
	}
	if err := registry.ValidateLedger(next); err != nil {
		return err
	}
	current, err := store.ReadMigrationLedger(registry, registryDigest)
	if err != nil {
		return err
	}
	if current.Checkpointed != expected.Checkpointed ||
		!sameProtectedSnapshot(current.AuthoritySnapshot, expected.AuthoritySnapshot) ||
		!sameProtectedSnapshot(current.ProjectionSnapshot, expected.ProjectionSnapshot) {
		return ErrProtectedStoreStale
	}
	if !expected.Checkpointed {
		payload, _, err := MarshalLedgerV2(next, registry)
		if err != nil {
			return err
		}
		return store.CompareAndSwapLedger(expected.AuthoritySnapshot, payload)
	}
	checkpoint, err := advanceLedgerCheckpointV1(
		expected.Checkpoint, next, registry, registryDigest,
		expected.ProjectionSnapshot,
	)
	if err != nil {
		return err
	}
	payload, _, err := MarshalLedgerCheckpointV1(
		checkpoint, registry, registryDigest, expected.ProjectionSnapshot,
	)
	if err != nil {
		return err
	}
	return store.compareAndSwap(
		[]string{"release-transition", "v2"}, migrationLedgerCheckpointName,
		expected.AuthoritySnapshot, payload,
	)
}

// ConvertMigrationLedger atomically publishes or replaces the authoritative
// compact checkpoint. The V2 projection is never rewritten. The caller must
// hold the transition lock and establish readiness/no in-flight journal pins.
func (store *POSIXV2Store) ConvertMigrationLedger(
	expected MigrationLedgerSnapshot,
	predecessor RegistryV2,
	predecessorDigest Fingerprint,
	registry RegistryV2,
	registryDigest Fingerprint,
) error {
	if store == nil {
		return errors.New("release transition store is required")
	}
	// A crash after checkpoint publication is already an authority switch. If
	// the exact requested successor is present, treat the retry as complete.
	if published, readErr := store.ReadMigrationLedger(registry, registryDigest); readErr == nil &&
		published.Checkpointed && sameProtectedSnapshot(published.ProjectionSnapshot, expected.ProjectionSnapshot) {
		want, wantErr := ledgerAtRegistryEpochs(expected.Ledger, registry)
		if wantErr == nil && sameLedgerV2(published.Ledger, want) &&
			checkpointFullyCompactsLedger(published.Checkpoint, want, registry) {
			return nil
		}
	}
	current, err := store.ReadMigrationLedger(predecessor, predecessorDigest)
	if err != nil {
		return err
	}
	if current.Checkpointed != expected.Checkpointed ||
		!sameProtectedSnapshot(current.AuthoritySnapshot, expected.AuthoritySnapshot) ||
		!sameProtectedSnapshot(current.ProjectionSnapshot, expected.ProjectionSnapshot) {
		return ErrProtectedStoreStale
	}
	for domain, retired := range registry.RetiredPrefixes {
		proof, proofErr := predecessor.MigrationPrefixDigest(domain, retired.ThroughEpoch)
		if proofErr != nil || proof != retired.Digest {
			return invalid("retired migration prefix for domain %q does not match predecessor history", domain)
		}
	}
	ledger := current.Ledger
	ledgerForRegistry, err := ledgerAtRegistryEpochs(ledger, registry)
	if err != nil {
		return err
	}
	legacyEpochs := make(map[string]int, len(registry.MinimumEpochs))
	if current.Checkpointed {
		legacyEpochs = cloneEpochs(current.Checkpoint.LegacyProjection.Epochs)
	} else {
		for domain, state := range ledger.Domains {
			legacyEpochs[domain] = state.Epoch
		}
	}
	for domain, minimum := range registry.MinimumEpochs {
		state, exists := ledger.Domains[domain]
		if !exists || state.Epoch < minimum || state.Epoch > registry.CurrentEpochs[domain] {
			return invalid("migration domain %q is outside the checkpoint registry floor", domain)
		}
	}
	if err := registry.ValidateLedger(ledgerForRegistry); err != nil {
		return err
	}
	checkpoint, err := newLedgerCheckpointV1(
		ledgerForRegistry, registry, registryDigest, current.ProjectionSnapshot, legacyEpochs,
	)
	if err != nil {
		return err
	}
	payload, _, err := MarshalLedgerCheckpointV1(
		checkpoint, registry, registryDigest, current.ProjectionSnapshot,
	)
	if err != nil {
		return err
	}
	if current.Checkpointed {
		return store.compareAndSwap(
			[]string{"release-transition", "v2"}, migrationLedgerCheckpointName,
			current.AuthoritySnapshot, payload,
		)
	}
	return store.createImmutable(
		[]string{"release-transition", "v2"}, migrationLedgerCheckpointName, payload,
	)
}

func checkpointFullyCompactsLedger(
	checkpoint LedgerCheckpointV1,
	ledger LedgerV2,
	registry RegistryV2,
) bool {
	if len(checkpoint.Domains) != len(ledger.Domains) {
		return false
	}
	for domain, state := range ledger.Domains {
		compacted, exists := checkpoint.Domains[domain]
		if !exists || compacted.Epoch != state.Epoch || compacted.CompactedThrough != state.Epoch ||
			len(compacted.AppliedSuffix) != 0 {
			return false
		}
		prefix, err := registry.MigrationPrefixDigest(domain, state.Epoch)
		if err != nil || compacted.PrefixDigest != prefix {
			return false
		}
	}
	return true
}

func ledgerAtRegistryEpochs(source LedgerV2, registry RegistryV2) (LedgerV2, error) {
	ledger := LedgerV2{SchemaVersion: LedgerSchemaV2, Domains: make(map[string]DomainLedgerV2, len(registry.MinimumEpochs))}
	for domain := range source.Domains {
		if _, exists := registry.MinimumEpochs[domain]; !exists {
			return LedgerV2{}, invalid("migration ledger contains removed domain %q", domain)
		}
	}
	for domain, minimum := range registry.MinimumEpochs {
		state, exists := source.Domains[domain]
		if !exists {
			if minimum != 1 || registry.RetiredPrefixes[domain].Digest != "" {
				return LedgerV2{}, invalid("migration ledger cannot prove newly added domain %q", domain)
			}
			state = DomainLedgerV2{Epoch: minimum}
		}
		if state.Epoch < minimum || state.Epoch > registry.CurrentEpochs[domain] {
			return LedgerV2{}, invalid("migration domain %q is outside the checkpoint registry floor", domain)
		}
		ledger.Domains[domain] = DomainLedgerV2{
			Epoch: state.Epoch, Applied: registry.appliedPrefix(domain, state.Epoch),
		}
	}
	if err := registry.ValidateLedger(ledger); err != nil {
		return LedgerV2{}, err
	}
	return ledger, nil
}

func sameLedgerV2(left, right LedgerV2) bool {
	if left.SchemaVersion != right.SchemaVersion || len(left.Domains) != len(right.Domains) {
		return false
	}
	for domain, state := range left.Domains {
		other, exists := right.Domains[domain]
		if !exists || state.Epoch != other.Epoch || !slices.Equal(state.Applied, other.Applied) {
			return false
		}
	}
	return true
}

func advanceLedgerCheckpointV1(
	checkpoint LedgerCheckpointV1,
	next LedgerV2,
	registry RegistryV2,
	registryDigest Fingerprint,
	projection ProtectedSnapshot,
) (LedgerCheckpointV1, error) {
	if err := checkpoint.Validate(registry, registryDigest, projection); err != nil {
		return LedgerCheckpointV1{}, err
	}
	if err := registry.ValidateLedger(next); err != nil {
		return LedgerCheckpointV1{}, err
	}
	advanced := LedgerCheckpointV1{
		SchemaVersion: LedgerCheckpointSchemaV1, RegistryDigest: registryDigest,
		LegacyProjection: LedgerProjectionBindingV1{
			Exists:      checkpoint.LegacyProjection.Exists,
			Fingerprint: checkpoint.LegacyProjection.Fingerprint,
			Epochs:      cloneEpochs(checkpoint.LegacyProjection.Epochs),
		},
		Domains: make(map[string]DomainCheckpointV1, len(next.Domains)),
	}
	for domain, state := range next.Domains {
		prior, exists := checkpoint.Domains[domain]
		if !exists {
			minimum := registry.MinimumEpochs[domain]
			if minimum != 1 || state.Epoch < minimum || registry.RetiredPrefixes[domain].Digest != "" {
				return LedgerCheckpointV1{}, invalid("migration checkpoint cannot prove new domain %q", domain)
			}
			prefixDigest, err := registry.MigrationPrefixDigest(domain, minimum)
			if err != nil {
				return LedgerCheckpointV1{}, err
			}
			prior = DomainCheckpointV1{CompactedThrough: minimum, PrefixDigest: prefixDigest}
		}
		if state.Epoch < prior.CompactedThrough {
			return LedgerCheckpointV1{}, invalid("migration checkpoint domain %q moved behind its compacted prefix", domain)
		}
		advanced.Domains[domain] = DomainCheckpointV1{
			Epoch: state.Epoch, CompactedThrough: prior.CompactedThrough,
			PrefixDigest:  prior.PrefixDigest,
			AppliedSuffix: registry.appliedSuffix(domain, prior.CompactedThrough, state.Epoch),
		}
	}
	if err := advanced.Validate(registry, registryDigest, projection); err != nil {
		return LedgerCheckpointV1{}, err
	}
	return advanced, nil
}

// DiffersFromProjection reports whether a frozen reader would see stale epochs.
func (snapshot MigrationLedgerSnapshot) DiffersFromProjection(registry RegistryV2) bool {
	if !snapshot.Checkpointed {
		return false
	}
	for domain, state := range snapshot.Checkpoint.Domains {
		projectionEpoch, exists := snapshot.Checkpoint.LegacyProjection.Epochs[domain]
		if !exists {
			projectionEpoch = registry.MinimumEpochs[domain]
		}
		if state.Epoch != projectionEpoch {
			return true
		}
	}
	return false
}
