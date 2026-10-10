package releasetransition

import (
	"encoding/json"
	"fmt"
	"slices"
)

const (
	LedgerCheckpointSchemaV1 = 1
	MaxLedgerCheckpointBytes = 1 << 20
)

// LedgerCheckpointV1 stores the completed migration prefix by digest and only
// retains an applied suffix after the compaction point. The legacy V2 ledger
// remains a pinned, read-only projection for compatible retained readers.
type LedgerCheckpointV1 struct {
	SchemaVersion    int                           `json:"schemaVersion"`
	RegistryDigest   Fingerprint                   `json:"registryDigest"`
	LegacyProjection LedgerProjectionBindingV1     `json:"legacyProjection"`
	Domains          map[string]DomainCheckpointV1 `json:"domains"`
}

type LedgerProjectionBindingV1 struct {
	Exists      bool           `json:"exists"`
	Fingerprint Fingerprint    `json:"fingerprint,omitempty"`
	Epochs      map[string]int `json:"epochs"`
}

type DomainCheckpointV1 struct {
	Epoch            int         `json:"epoch"`
	CompactedThrough int         `json:"compactedThrough"`
	PrefixDigest     Fingerprint `json:"prefixDigest"`
	AppliedSuffix    []string    `json:"appliedSuffix"`
}

type MigrationLedgerSnapshot struct {
	Ledger             LedgerV2
	Checkpoint         LedgerCheckpointV1
	AuthoritySnapshot  ProtectedSnapshot
	ProjectionSnapshot ProtectedSnapshot
	Checkpointed       bool
}

// RequiresCheckpointPublication reports unconverted or uncompacted history.
// Mutation still requires the writer capability and the exact authorization.
func (snapshot MigrationLedgerSnapshot) RequiresCheckpointPublication() bool {
	return !snapshot.Checkpointed || ledgerCheckpointHasSuffix(snapshot.Checkpoint)
}

func ParseLedgerCheckpointV1(
	payload []byte,
	registry RegistryV2,
	registryDigest Fingerprint,
	projection ProtectedSnapshot,
) (LedgerCheckpointV1, Fingerprint, error) {
	var checkpoint LedgerCheckpointV1
	if err := decodeBoundedRecord(payload, MaxLedgerCheckpointBytes, &checkpoint); err != nil {
		return LedgerCheckpointV1{}, "", fmt.Errorf("%w: decode migration ledger checkpoint: %v", ErrInvalid, err)
	}
	if err := checkpoint.Validate(registry, registryDigest, projection); err != nil {
		return LedgerCheckpointV1{}, "", err
	}
	return checkpoint, fingerprintPayload(payload), nil
}

func MarshalLedgerCheckpointV1(
	checkpoint LedgerCheckpointV1,
	registry RegistryV2,
	registryDigest Fingerprint,
	projection ProtectedSnapshot,
) ([]byte, Fingerprint, error) {
	if err := checkpoint.Validate(registry, registryDigest, projection); err != nil {
		return nil, "", err
	}
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, "", err
	}
	payload = append(payload, '\n')
	return payload, fingerprintPayload(payload), nil
}

func (checkpoint LedgerCheckpointV1) Validate(
	registry RegistryV2,
	registryDigest Fingerprint,
	projection ProtectedSnapshot,
) error {
	if checkpoint.SchemaVersion != LedgerCheckpointSchemaV1 {
		return invalid("unsupported migration ledger checkpoint schema %d", checkpoint.SchemaVersion)
	}
	if err := validateFingerprint(checkpoint.RegistryDigest, "checkpoint registry digest"); err != nil {
		return err
	}
	if err := validateFingerprint(registryDigest, "current registry digest"); err != nil {
		return err
	}
	if checkpoint.Domains == nil {
		return invalid("migration ledger checkpoint domains are required")
	}
	if checkpoint.LegacyProjection.Epochs == nil {
		return invalid("legacy ledger projection epochs are required")
	}
	if checkpoint.LegacyProjection.Exists {
		if err := validateFingerprint(checkpoint.LegacyProjection.Fingerprint, "legacy ledger projection fingerprint"); err != nil {
			return err
		}
	} else if checkpoint.LegacyProjection.Fingerprint != "" {
		return invalid("absent legacy ledger projection has a fingerprint")
	}
	if !validProtectedSnapshot(projection) {
		return invalid("legacy ledger projection snapshot is malformed")
	}
	if projection.Exists != checkpoint.LegacyProjection.Exists ||
		(projection.Exists && projection.Fingerprint != checkpoint.LegacyProjection.Fingerprint) {
		return invalid("legacy ledger projection changed after checkpoint publication")
	}
	if projection.Exists {
		epochs, err := decodeLegacyLedgerEpochs(projection.Payload)
		if err != nil {
			return err
		}
		if !sameEpochs(epochs, checkpoint.LegacyProjection.Epochs) {
			return invalid("legacy ledger projection epochs do not match its pinned bytes")
		}
	}
	for domain, state := range checkpoint.Domains {
		minimum, exists := registry.MinimumEpochs[domain]
		if !exists {
			return invalid("migration checkpoint contains unknown domain %q", domain)
		}
		if state.Epoch < minimum || state.Epoch > registry.CurrentEpochs[domain] ||
			state.CompactedThrough < minimum || state.CompactedThrough > state.Epoch {
			return invalid("migration checkpoint domain %q has an invalid epoch", domain)
		}
		legacyEpoch, exists := checkpoint.LegacyProjection.Epochs[domain]
		if exists && (legacyEpoch < 1 || legacyEpoch > state.Epoch) {
			return invalid("migration checkpoint domain %q has an invalid legacy epoch", domain)
		}
		if !exists && (minimum != 1 || registry.RetiredPrefixes[domain].Digest != "") {
			return invalid("migration checkpoint domain %q is absent from pinned legacy history", domain)
		}
		prefixDigest, err := registry.MigrationPrefixDigest(domain, state.CompactedThrough)
		if err != nil || prefixDigest != state.PrefixDigest {
			return invalid("migration checkpoint domain %q prefix proof does not match registry", domain)
		}
		if !slices.Equal(state.AppliedSuffix, registry.appliedSuffix(domain, state.CompactedThrough, state.Epoch)) {
			return invalid("migration checkpoint domain %q applied suffix is not the exact registry path", domain)
		}
	}
	for domain := range checkpoint.LegacyProjection.Epochs {
		if _, exists := checkpoint.Domains[domain]; !exists {
			return invalid("legacy ledger projection has unknown domain %q", domain)
		}
	}
	for domain, minimum := range registry.MinimumEpochs {
		if _, exists := checkpoint.Domains[domain]; exists {
			continue
		}
		if minimum != 1 || registry.RetiredPrefixes[domain].Digest != "" {
			return invalid("migration checkpoint cannot prove newly added domain %q", domain)
		}
	}
	return nil
}

func (checkpoint LedgerCheckpointV1) LedgerView(registry RegistryV2) (LedgerV2, error) {
	if checkpoint.SchemaVersion != LedgerCheckpointSchemaV1 {
		return LedgerV2{}, invalid("unsupported migration ledger checkpoint schema %d", checkpoint.SchemaVersion)
	}
	ledger := BaselineLedgerV2(registry)
	for domain, state := range checkpoint.Domains {
		if _, exists := registry.MinimumEpochs[domain]; !exists {
			return LedgerV2{}, invalid("migration checkpoint contains unknown domain %q", domain)
		}
		ledger.Domains[domain] = DomainLedgerV2{
			Epoch: state.Epoch, Applied: nonNilIDs(registry.appliedPrefix(domain, state.Epoch)),
		}
	}
	if err := registry.ValidateLedger(ledger); err != nil {
		return LedgerV2{}, err
	}
	return ledger, nil
}

func nonNilIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func decodeLegacyLedgerEpochs(payload []byte) (map[string]int, error) {
	var record struct {
		SchemaVersion int                       `json:"schemaVersion"`
		Domains       map[string]DomainLedgerV2 `json:"domains"`
	}
	if err := decodeBoundedRecord(payload, MaxLedgerV2Bytes, &record); err != nil {
		return nil, invalid("pinned legacy ledger projection is malformed")
	}
	if record.SchemaVersion != LedgerSchemaV2 || record.Domains == nil {
		return nil, invalid("pinned legacy ledger projection has an invalid shape")
	}
	epochs := make(map[string]int, len(record.Domains))
	for domain, state := range record.Domains {
		if err := validateSafeID(domain, "legacy ledger domain"); err != nil {
			return nil, err
		}
		if state.Epoch < 1 {
			return nil, invalid("pinned legacy ledger projection has an invalid epoch")
		}
		epochs[domain] = state.Epoch
	}
	return epochs, nil
}

func sameEpochs(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for domain, epoch := range left {
		if right[domain] != epoch {
			return false
		}
	}
	return true
}

func newLedgerCheckpointV1(
	ledger LedgerV2,
	registry RegistryV2,
	registryDigest Fingerprint,
	projection ProtectedSnapshot,
	legacyEpochs map[string]int,
) (LedgerCheckpointV1, error) {
	if err := registry.ValidateLedger(ledger); err != nil {
		return LedgerCheckpointV1{}, err
	}
	checkpoint := LedgerCheckpointV1{
		SchemaVersion:  LedgerCheckpointSchemaV1,
		RegistryDigest: registryDigest,
		LegacyProjection: LedgerProjectionBindingV1{
			Exists: projection.Exists,
			Epochs: cloneEpochs(legacyEpochs),
		},
		Domains: make(map[string]DomainCheckpointV1, len(ledger.Domains)),
	}
	if projection.Exists {
		checkpoint.LegacyProjection.Fingerprint = projection.Fingerprint
	}
	for domain, state := range ledger.Domains {
		prefixDigest, err := registry.MigrationPrefixDigest(domain, state.Epoch)
		if err != nil {
			return LedgerCheckpointV1{}, err
		}
		checkpoint.Domains[domain] = DomainCheckpointV1{
			Epoch: state.Epoch, CompactedThrough: state.Epoch, PrefixDigest: prefixDigest,
			AppliedSuffix: []string{},
		}
	}
	return checkpoint, nil
}

func (registry RegistryV2) appliedSuffix(domain string, afterEpoch, throughEpoch int) []string {
	var applied []string
	for _, migration := range registry.Migrations {
		if migration.Domain == domain && migration.FromEpoch >= afterEpoch && migration.ToEpoch <= throughEpoch {
			applied = append(applied, migration.ID)
		}
	}
	return applied
}

func cloneEpochs(source map[string]int) map[string]int {
	result := make(map[string]int, len(source))
	for domain, epoch := range source {
		result[domain] = epoch
	}
	return result
}

func ledgerCheckpointHasSuffix(checkpoint LedgerCheckpointV1) bool {
	for _, domain := range checkpoint.Domains {
		if domain.Epoch > domain.CompactedThrough || len(domain.AppliedSuffix) != 0 {
			return true
		}
	}
	return false
}
