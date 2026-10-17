package releasetransition

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Subyard/Subyard/internal/migrationhistory"
)

const (
	LedgerSchemaV2   = 2
	MaxLedgerV2Bytes = 1 << 20
)

type LedgerV2 struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Domains       map[string]DomainLedgerV2 `json:"domains"`
}

type DomainLedgerV2 = migrationhistory.DomainState

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

func (registry RegistryV2) historyAuthority(ledger LedgerV2) (migrationhistory.Registry, migrationhistory.Authority, error) {
	owner, err := registry.historyRegistry()
	if err != nil {
		return owner, migrationhistory.Authority{}, err
	}
	if ledger.SchemaVersion != LedgerSchemaV2 {
		return owner, migrationhistory.Authority{}, invalid("unsupported ledger schema %d", ledger.SchemaVersion)
	}
	authority, err := owner.History(ledger.Domains)
	if err != nil {
		return owner, authority, invalid("%v", err)
	}
	return owner, authority, nil
}
func ledgerFromAuthority(authority migrationhistory.Authority) LedgerV2 {
	return LedgerV2{SchemaVersion: LedgerSchemaV2, Domains: authority.Domains()}
}
func (registry RegistryV2) ValidateLedger(ledger LedgerV2) error {
	_, _, err := registry.historyAuthority(ledger)
	return err
}
func (registry RegistryV2) PendingPath(ledger LedgerV2) ([]MigrationDefinitionV2, error) {
	owner, authority, err := registry.historyAuthority(ledger)
	if err != nil {
		return nil, err
	}
	path, err := owner.Pending(authority)
	if err != nil {
		return nil, invalid("%v", err)
	}
	return path, nil
}
func (ledger LedgerV2) DomainState(registry RegistryV2, domain string) (DomainLedgerV2, error) {
	_, authority, err := registry.historyAuthority(ledger)
	if err != nil {
		return DomainLedgerV2{}, err
	}
	state, ok := authority.Domains()[domain]
	if !ok {
		return DomainLedgerV2{}, invalid("unknown migration domain %q", domain)
	}
	return state, nil
}

// Advance selects the canonical next edge from validated authority. It never
// mutates the caller's maps, definition dependencies or applied histories.
func (ledger LedgerV2) Advance(registry RegistryV2, migration MigrationDefinitionV2) (LedgerV2, error) {
	edge, err := nextLedgerEdge(registry, ledger, migration)
	if err != nil {
		return LedgerV2{}, err
	}
	return ledgerFromAuthority(edge.After()), nil
}
func sameMigrationDefinition(left, right MigrationDefinitionV2) bool {
	return left.ID == right.ID && left.Domain == right.Domain && left.FromEpoch == right.FromEpoch && left.ToEpoch == right.ToEpoch && left.Kind == right.Kind && slices.Equal(left.DependsOn, right.DependsOn)
}
func (registry RegistryV2) appliedPrefix(domain string, epoch int) []string {
	owner, err := registry.historyRegistry()
	if err != nil {
		return nil
	}
	applied, _ := owner.Applied(domain, registry.MinimumEpochs[domain], epoch)
	return applied
}
