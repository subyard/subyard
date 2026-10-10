package releasetransition

import (
	"encoding/json"
	"fmt"
	"slices"
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

type MigrationDefinitionV2 struct {
	ID        string   `json:"id"`
	Domain    string   `json:"domain"`
	FromEpoch int      `json:"fromEpoch"`
	ToEpoch   int      `json:"toEpoch"`
	Kind      string   `json:"kind"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

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

func (registry RegistryV2) Validate(catalog CapabilityCatalog) error {
	if registry.SchemaVersion != RegistrySchemaV2 {
		return invalid("unsupported registry schema %d", registry.SchemaVersion)
	}
	if len(registry.MinimumEpochs) == 0 || len(registry.CurrentEpochs) == 0 {
		return invalid("registry domain epochs are required")
	}
	if len(registry.MinimumEpochs) != len(registry.CurrentEpochs) {
		return invalid("registry minimum and current domains differ")
	}
	for domain, minimum := range registry.MinimumEpochs {
		if err := validateSafeID(domain, "registry domain"); err != nil {
			return err
		}
		current, exists := registry.CurrentEpochs[domain]
		if !exists {
			return invalid("registry current epoch is missing domain %q", domain)
		}
		if minimum < 1 || current < minimum {
			return invalid("registry domain %q has invalid epoch range", domain)
		}
	}
	for domain := range registry.CurrentEpochs {
		if _, exists := registry.MinimumEpochs[domain]; !exists {
			return invalid("registry current epoch has unknown domain %q", domain)
		}
	}
	for domain, prefix := range registry.RetiredPrefixes {
		minimum, exists := registry.MinimumEpochs[domain]
		if !exists || prefix.ThroughEpoch != minimum {
			return invalid("registry retired prefix for domain %q has an invalid floor", domain)
		}
		if err := validateFingerprint(prefix.Digest, "retired migration prefix digest"); err != nil {
			return err
		}
	}

	ids := make(map[string]int, len(registry.Migrations))
	for index, migration := range registry.Migrations {
		if err := validateSafeID(migration.ID, "migration ID"); err != nil {
			return err
		}
		if _, exists := ids[migration.ID]; exists {
			return invalid("duplicate migration ID %q", migration.ID)
		}
		ids[migration.ID] = index
		if _, exists := registry.MinimumEpochs[migration.Domain]; !exists {
			return invalid("migration %q has unknown domain %q", migration.ID, migration.Domain)
		}
		if migration.ToEpoch != migration.FromEpoch+1 {
			return invalid("migration %q must advance exactly one epoch", migration.ID)
		}
		if !catalog.Supports(migration.Kind, migration.Domain) {
			return invalid("migration %q has unsupported kind %q", migration.ID, migration.Kind)
		}
		seenDependencies := make(map[string]struct{}, len(migration.DependsOn))
		for _, dependency := range migration.DependsOn {
			if err := validateSafeID(dependency, "migration dependency"); err != nil {
				return err
			}
			if _, duplicate := seenDependencies[dependency]; duplicate {
				return invalid("migration %q has duplicate dependency %q", migration.ID, dependency)
			}
			seenDependencies[dependency] = struct{}{}
		}
	}
	for _, migration := range registry.Migrations {
		for _, dependency := range migration.DependsOn {
			if _, exists := ids[dependency]; !exists {
				return invalid("migration %q has unknown dependency %q", migration.ID, dependency)
			}
		}
	}
	if err := validateMigrationDependencyCycles(registry.Migrations, ids); err != nil {
		return err
	}
	for index, migration := range registry.Migrations {
		for _, dependency := range migration.DependsOn {
			if ids[dependency] >= index {
				return invalid("migration %q dependency %q is not ordered before it", migration.ID, dependency)
			}
		}
	}

	expected := make(map[string]int, len(registry.MinimumEpochs))
	for domain, minimum := range registry.MinimumEpochs {
		expected[domain] = minimum
	}
	for _, migration := range registry.Migrations {
		if migration.FromEpoch != expected[migration.Domain] {
			return invalid(
				"migration %q starts domain %q at epoch %d; expected %d",
				migration.ID, migration.Domain, migration.FromEpoch, expected[migration.Domain],
			)
		}
		expected[migration.Domain] = migration.ToEpoch
	}
	for domain, current := range registry.CurrentEpochs {
		if expected[domain] != current {
			return invalid("registry domain %q path ends at epoch %d; current is %d", domain, expected[domain], current)
		}
	}
	return nil
}

// MigrationPrefixDigest binds the ordered migration definitions completed
// through an epoch. Previously retired history is represented by its digest,
// so repeated retirement does not retain an ever-growing definition list.
func (registry RegistryV2) MigrationPrefixDigest(domain string, throughEpoch int) (Fingerprint, error) {
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
	minimum, exists := registry.MinimumEpochs[domain]
	if !exists {
		return nil, invalid("unknown migration domain %q", domain)
	}
	current := registry.CurrentEpochs[domain]
	if fromEpoch < minimum {
		return nil, invalid("domain %q epoch %d is below supported minimum %d", domain, fromEpoch, minimum)
	}
	if fromEpoch > current {
		return nil, invalid("domain %q epoch %d is newer than current %d", domain, fromEpoch, current)
	}
	path := make([]MigrationDefinitionV2, 0, current-fromEpoch)
	for _, migration := range registry.Migrations {
		if migration.Domain == domain && migration.FromEpoch >= fromEpoch {
			migration.DependsOn = slices.Clone(migration.DependsOn)
			path = append(path, migration)
		}
	}
	return path, nil
}

func validateMigrationDependencyCycles(migrations []MigrationDefinitionV2, ids map[string]int) error {
	const (
		unvisited = iota
		visiting
		visited
	)
	states := make([]int, len(migrations))
	var visit func(int) error
	visit = func(index int) error {
		switch states[index] {
		case visiting:
			return invalid("migration dependencies contain a cycle")
		case visited:
			return nil
		}
		states[index] = visiting
		for _, dependency := range migrations[index].DependsOn {
			if err := visit(ids[dependency]); err != nil {
				return err
			}
		}
		states[index] = visited
		return nil
	}
	for index := range migrations {
		if err := visit(index); err != nil {
			return err
		}
	}
	return nil
}
