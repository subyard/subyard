package releasetransition

import (
	"encoding/json"
	"fmt"
	"slices"
)

const (
	LedgerSchemaV2   = 2
	MaxLedgerV2Bytes = 1 << 20
)

type LedgerV2 struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Domains       map[string]DomainLedgerV2 `json:"domains"`
}

type DomainLedgerV2 struct {
	Epoch   int      `json:"epoch"`
	Applied []string `json:"applied"`
}

func ParseLedgerV2(payload []byte, registry RegistryV2) (LedgerV2, Fingerprint, error) {
	var ledger LedgerV2
	if err := decodeBoundedRecord(payload, MaxLedgerV2Bytes, &ledger); err != nil {
		return LedgerV2{}, "", fmt.Errorf("%w: decode release transition ledger: %v", ErrInvalid, err)
	}
	if err := registry.ValidateLedger(ledger); err != nil {
		return LedgerV2{}, "", err
	}
	return ledger, fingerprintPayload(payload), nil
}

func MarshalLedgerV2(ledger LedgerV2, registry RegistryV2) ([]byte, Fingerprint, error) {
	if err := registry.ValidateLedger(ledger); err != nil {
		return nil, "", err
	}
	payload, err := json.Marshal(ledger)
	if err != nil {
		return nil, "", err
	}
	payload = append(payload, '\n')
	return payload, fingerprintPayload(payload), nil
}

func BaselineLedgerV2(registry RegistryV2) LedgerV2 {
	ledger := LedgerV2{
		SchemaVersion: LedgerSchemaV2,
		Domains:       make(map[string]DomainLedgerV2, len(registry.MinimumEpochs)),
	}
	for domain, minimum := range registry.MinimumEpochs {
		ledger.Domains[domain] = DomainLedgerV2{Epoch: minimum, Applied: []string{}}
	}
	return ledger
}

func (registry RegistryV2) ValidateLedger(ledger LedgerV2) error {
	if ledger.SchemaVersion != LedgerSchemaV2 {
		return invalid("unsupported ledger schema %d", ledger.SchemaVersion)
	}
	if ledger.Domains == nil {
		return invalid("ledger domains are required")
	}
	if len(ledger.Domains) != len(registry.MinimumEpochs) {
		return invalid("ledger domains do not exactly match registry domains")
	}
	for domain := range ledger.Domains {
		if _, exists := registry.MinimumEpochs[domain]; !exists {
			return invalid("ledger contains unknown domain %q", domain)
		}
	}
	for domain, state := range ledger.Domains {
		minimum := registry.MinimumEpochs[domain]
		current := registry.CurrentEpochs[domain]
		if state.Epoch < minimum || state.Epoch > current {
			return invalid("ledger domain %q epoch %d is outside supported range", domain, state.Epoch)
		}
		expected := registry.appliedPrefix(domain, state.Epoch)
		if !slices.Equal(state.Applied, expected) {
			return invalid("ledger domain %q applied history is not the exact registry prefix", domain)
		}
	}
	applied := ledgerAppliedIDs(ledger)
	for _, migration := range registry.Migrations {
		if _, completed := applied[migration.ID]; !completed {
			continue
		}
		for _, dependency := range migration.DependsOn {
			if _, completed := applied[dependency]; !completed {
				return invalid(
					"ledger migration %q is applied before dependency %q",
					migration.ID, dependency,
				)
			}
		}
	}
	return nil
}

func (registry RegistryV2) PendingPath(ledger LedgerV2) ([]MigrationDefinitionV2, error) {
	if err := registry.ValidateLedger(ledger); err != nil {
		return nil, err
	}
	applied := ledgerAppliedIDs(ledger)
	path := make([]MigrationDefinitionV2, 0, len(registry.Migrations)-len(applied))
	for _, migration := range registry.Migrations {
		if _, completed := applied[migration.ID]; completed {
			continue
		}
		migration.DependsOn = slices.Clone(migration.DependsOn)
		path = append(path, migration)
	}
	return path, nil
}

func (ledger LedgerV2) DomainState(registry RegistryV2, domain string) (DomainLedgerV2, error) {
	_, exists := registry.MinimumEpochs[domain]
	if !exists {
		return DomainLedgerV2{}, invalid("unknown migration domain %q", domain)
	}
	if err := registry.ValidateLedger(ledger); err != nil {
		return DomainLedgerV2{}, err
	}
	state, exists := ledger.Domains[domain]
	if !exists {
		return DomainLedgerV2{}, invalid("ledger is missing domain %q", domain)
	}
	state.Applied = slices.Clone(state.Applied)
	return state, nil
}

// Advance returns a new ledger with exactly the next registry migration
// applied. It never mutates the caller's maps or histories.
func (ledger LedgerV2) Advance(
	registry RegistryV2,
	migration MigrationDefinitionV2,
) (LedgerV2, error) {
	pending, err := registry.PendingPath(ledger)
	if err != nil {
		return LedgerV2{}, err
	}
	if len(pending) == 0 || pending[0].ID != migration.ID ||
		pending[0].Domain != migration.Domain ||
		pending[0].FromEpoch != migration.FromEpoch || pending[0].ToEpoch != migration.ToEpoch ||
		pending[0].Kind != migration.Kind || !slices.Equal(pending[0].DependsOn, migration.DependsOn) {
		return LedgerV2{}, invalid("migration %q is not the exact next ledger transition", migration.ID)
	}
	advanced := LedgerV2{
		SchemaVersion: ledger.SchemaVersion,
		Domains:       make(map[string]DomainLedgerV2, len(ledger.Domains)),
	}
	for domain, state := range ledger.Domains {
		advanced.Domains[domain] = DomainLedgerV2{
			Epoch: state.Epoch, Applied: slices.Clone(state.Applied),
		}
	}
	state := advanced.Domains[migration.Domain]
	state.Epoch = migration.ToEpoch
	state.Applied = append(state.Applied, migration.ID)
	advanced.Domains[migration.Domain] = state
	if err := registry.ValidateLedger(advanced); err != nil {
		return LedgerV2{}, err
	}
	return advanced, nil
}

func (registry RegistryV2) appliedPrefix(domain string, epoch int) []string {
	var applied []string
	for _, migration := range registry.Migrations {
		if migration.Domain == domain && migration.ToEpoch <= epoch {
			applied = append(applied, migration.ID)
		}
	}
	return applied
}

func ledgerAppliedIDs(ledger LedgerV2) map[string]struct{} {
	applied := make(map[string]struct{})
	for _, state := range ledger.Domains {
		for _, migration := range state.Applied {
			applied[migration] = struct{}{}
		}
	}
	return applied
}
