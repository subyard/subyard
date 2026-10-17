package releasetransition

import (
	"encoding/json"
	"fmt"

	"github.com/Subyard/Subyard/internal/migrationhistory"
)

const (
	RegistrySchemaV2   = 2
	MaxRegistryV2Bytes = 1 << 20
)

type RegistryV2 struct {
	SchemaVersion   int                                 `json:"schemaVersion"`
	MinimumEpochs   map[string]int                      `json:"minimumEpochs"`
	CurrentEpochs   map[string]int                      `json:"currentEpochs"`
	Migrations      []MigrationDefinitionV2             `json:"migrations"`
	RetiredPrefixes map[string]RetiredMigrationPrefixV2 `json:"retiredPrefixes,omitempty"`
}

// RetiredMigrationPrefixV2 describes completed history whose executable
// definitions have left the live migration path. Digest binds the retired
// definitions without keeping them in the active registry.
type RetiredMigrationPrefixV2 struct {
	ThroughEpoch int         `json:"throughEpoch"`
	Digest       Fingerprint `json:"digest"`
}

type MigrationDefinitionV2 = migrationhistory.Migration

func ParseRegistryV2(payload []byte, catalog CapabilityCatalog) (RegistryV2, Fingerprint, error) {
	var registry RegistryV2
	if err := decodeBoundedRecord(payload, MaxRegistryV2Bytes, &registry); err != nil {
		return RegistryV2{}, "", fmt.Errorf(
			"%w: %w: decode release transition registry: %v",
			ErrRegistryInvalid, ErrInvalid, err,
		)
	}
	if err := registry.Validate(catalog); err != nil {
		return RegistryV2{}, "", fmt.Errorf("%w: %w", ErrRegistryInvalid, err)
	}
	return registry, fingerprintPayload(payload), nil
}

// historyRegistry validates semantics independently of the adapter's compiled
// capability policy. Every public DTO boundary reconstructs a checked owner.
func (registry RegistryV2) historyRegistry() (migrationhistory.Registry, error) {
	if registry.SchemaVersion != RegistrySchemaV2 {
		return migrationhistory.Registry{}, invalid("unsupported registry schema %d", registry.SchemaVersion)
	}
	for domain, prefix := range registry.RetiredPrefixes {
		minimum, exists := registry.MinimumEpochs[domain]
		if !exists || prefix.ThroughEpoch != minimum {
			return migrationhistory.Registry{}, invalid("registry retired prefix for domain %q has an invalid floor", domain)
		}
		if err := validateFingerprint(prefix.Digest, "retired migration prefix digest"); err != nil {
			return migrationhistory.Registry{}, err
		}
	}
	retired := make(map[string]string, len(registry.RetiredPrefixes))
	for domain, prefix := range registry.RetiredPrefixes {
		retired[domain] = string(prefix.Digest)
	}
	owner, err := migrationhistory.NewRegistry(registry.MinimumEpochs, registry.CurrentEpochs, registry.Migrations, retired)
	if err != nil {
		return migrationhistory.Registry{}, invalid("%v", err)
	}
	return owner, nil
}
func (registry RegistryV2) Validate(catalog CapabilityCatalog) error {
	if _, err := registry.historyRegistry(); err != nil {
		return err
	}
	for _, migration := range registry.Migrations {
		if !catalog.Supports(migration.Kind, migration.Domain) {
			return invalid("migration %q has unsupported kind %q", migration.ID, migration.Kind)
		}
	}
	return nil
}

// MigrationPrefixDigest binds the ordered migration definitions completed
// through an epoch. Previously retired history is represented by its digest,
// so repeated retirement does not retain an ever-growing definition list.
func (registry RegistryV2) MigrationPrefixDigest(domain string, throughEpoch int) (Fingerprint, error) {
	if _, err := registry.historyRegistry(); err != nil {
		return "", err
	}
	minimum, exists := registry.MinimumEpochs[domain]
	if !exists || throughEpoch < minimum || throughEpoch > registry.CurrentEpochs[domain] {
		return "", invalid("retired migration prefix is outside the registry path")
	}
	baseEpoch := minimum
	baseDigest, err := migrationPrefixSeed(domain, minimum)
	if err != nil {
		return "", err
	}
	if retired, exists := registry.RetiredPrefixes[domain]; exists {
		baseEpoch, baseDigest = retired.ThroughEpoch, retired.Digest
		if throughEpoch == baseEpoch {
			return baseDigest, nil
		}
	}
	if throughEpoch < baseEpoch {
		return "", invalid("retired migration prefix precedes existing checkpoint")
	}
	entries := make([]MigrationDefinitionV2, 0, throughEpoch-baseEpoch)
	for _, migration := range registry.Migrations {
		if migration.Domain == domain && migration.FromEpoch >= baseEpoch && migration.ToEpoch <= throughEpoch {
			entries = append(entries, migration)
		}
	}
	expectedEpoch := baseEpoch
	for _, migration := range entries {
		if migration.FromEpoch != expectedEpoch {
			return "", invalid("retired migration prefix has a gap")
		}
		expectedEpoch = migration.ToEpoch
	}
	if expectedEpoch != throughEpoch {
		return "", invalid("retired migration prefix is incomplete")
	}
	digest := baseDigest
	for _, migration := range entries {
		payload, err := json.Marshal(struct {
			Domain    string                `json:"domain"`
			FromEpoch int                   `json:"fromEpoch"`
			Previous  Fingerprint           `json:"previous"`
			Migration MigrationDefinitionV2 `json:"migration"`
		}{domain, migration.FromEpoch, digest, migration})
		if err != nil {
			return "", err
		}
		digest = fingerprintPayload(payload)
	}
	return digest, nil
}

func migrationPrefixSeed(domain string, epoch int) (Fingerprint, error) {
	payload, err := json.Marshal(struct {
		Domain string `json:"domain"`
		Epoch  int    `json:"epoch"`
	}{domain, epoch})
	if err != nil {
		return "", err
	}
	return fingerprintPayload(payload), nil
}

func (registry RegistryV2) Path(domain string, fromEpoch int) ([]MigrationDefinitionV2, error) {
	owner, err := registry.historyRegistry()
	if err != nil {
		return nil, err
	}
	path, err := owner.Path(domain, fromEpoch)
	if err != nil {
		return nil, invalid("%v", err)
	}
	return path, nil
}
