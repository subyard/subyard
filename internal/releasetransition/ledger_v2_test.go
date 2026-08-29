package releasetransition

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestBaselineLedgerV2StartsEveryDomainAtItsMinimum(t *testing.T) {
	registry, _, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	ledger := BaselineLedgerV2(registry)
	if ledger.Domains["settings"].Epoch != 1 || len(ledger.Domains["settings"].Applied) != 0 ||
		ledger.Domains["project-state"].Epoch != 1 {
		t.Fatalf("baseline ledger = %#v", ledger)
	}
	if err := registry.ValidateLedger(ledger); err != nil {
		t.Fatal(err)
	}
}

func TestParseLedgerV2AcceptsExactHistoricalPrefixes(t *testing.T) {
	registry, _, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{
      "schemaVersion":2,
      "domains":{
        "settings":{"epoch":2,"applied":["settings-v2"]},
        "project-state":{"epoch":2,"applied":["project-v2"]}
      }
    }`)
	ledger, digest, err := ParseLedgerV2(payload, registry)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Domains["settings"].Epoch != 2 || digest != fingerprintPayload(payload) {
		t.Fatalf("ledger=%#v digest=%q", ledger, digest)
	}
}

func TestLedgerV2RejectsForeignOrNonPrefixHistory(t *testing.T) {
	registry, _, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"schemaVersion":2,"domains":{"settings":{"epoch":2,"applied":["settings-v2"]},"project-state":{"epoch":1,"applied":[]}}}`
	tests := map[string]string{
		"unknown field":          strings.Replace(valid, `"schemaVersion":2`, `"schemaVersion":2,"layout":1`, 1),
		"trailing":               valid + `{}`,
		"schema":                 strings.Replace(valid, `"schemaVersion":2`, `"schemaVersion":1`, 1),
		"unknown domain":         strings.Replace(valid, `"domains":{`, `"domains":{"foreign":{"epoch":1,"applied":[]},`, 1),
		"below minimum":          strings.Replace(valid, `"epoch":2`, `"epoch":0`, 1),
		"future":                 strings.Replace(valid, `"epoch":2`, `"epoch":4`, 1),
		"foreign id":             strings.Replace(valid, `"settings-v2"`, `"foreign"`, 1),
		"missing prefix":         strings.Replace(valid, `"epoch":2,"applied":["settings-v2"]`, `"epoch":3,"applied":["settings-v3"]`, 1),
		"extra prefix":           strings.Replace(valid, `"settings-v2"]`, `"settings-v2","settings-v3"]`, 1),
		"dependency not applied": `{"schemaVersion":2,"domains":{"settings":{"epoch":1,"applied":[]},"project-state":{"epoch":2,"applied":["project-v2"]}}}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseLedgerV2([]byte(payload), registry); err == nil {
				t.Fatal("invalid ledger was accepted")
			}
		})
	}
	if _, _, err := ParseLedgerV2([]byte(strings.Repeat("x", MaxLedgerV2Bytes+1)), registry); err == nil {
		t.Fatal("unbounded ledger was accepted")
	}
}

func TestRegistryV2PendingPathUsesAllDomainEpochsInRegistryOrder(t *testing.T) {
	registry, _, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	path, err := registry.PendingPath(BaselineLedgerV2(registry))
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{path[0].ID, path[1].ID, path[2].ID}; !slices.Equal(
		got, []string{"settings-v2", "project-v2", "settings-v3"},
	) {
		t.Fatalf("baseline pending path = %v", got)
	}
	partial := LedgerV2{SchemaVersion: 2, Domains: map[string]DomainLedgerV2{
		"settings":      {Epoch: 2, Applied: []string{"settings-v2"}},
		"project-state": {Epoch: 2, Applied: []string{"project-v2"}},
	}}
	path, err = registry.PendingPath(partial)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 1 || path[0].ID != "settings-v3" {
		t.Fatalf("partial pending path = %#v", path)
	}
}

func TestLedgerV2RequiresExactRegistryDomainSet(t *testing.T) {
	registry, _, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseLedgerV2(
		[]byte(`{"schemaVersion":2,"domains":{"settings":{"epoch":1,"applied":[]}}}`),
		registry,
	); err == nil {
		t.Fatal("partial persisted ledger was accepted")
	}
	ledger := BaselineLedgerV2(registry)
	state, err := ledger.DomainState(registry, "project-state")
	if err != nil {
		t.Fatal(err)
	}
	if state.Epoch != 1 || len(state.Applied) != 0 {
		t.Fatalf("baseline domain state = %#v", state)
	}
	if _, err := ledger.DomainState(registry, "foreign"); err == nil {
		t.Fatal("unknown domain state was accepted")
	}
}

func TestMarshalLedgerV2ProducesCanonicalValidatedBytes(t *testing.T) {
	registry, _, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	ledger := LedgerV2{
		SchemaVersion: 2,
		Domains: map[string]DomainLedgerV2{
			"settings":      {Epoch: 2, Applied: []string{"settings-v2"}},
			"project-state": {Epoch: 1, Applied: []string{}},
		},
	}
	first, digest, err := MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := MarshalLedgerV2(LedgerV2{
		SchemaVersion: 2,
		Domains: map[string]DomainLedgerV2{
			"project-state": {Epoch: 1, Applied: []string{}},
			"settings":      {Epoch: 2, Applied: []string{"settings-v2"}},
		},
	}, registry)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || digest != fingerprintPayload(first) || first[len(first)-1] != '\n' {
		t.Fatalf("non-canonical ledger: first=%q second=%q digest=%q", first, second, digest)
	}
	ledger.Domains["settings"] = DomainLedgerV2{Epoch: 3, Applied: []string{"foreign"}}
	if _, _, err := MarshalLedgerV2(ledger, registry); err == nil {
		t.Fatal("invalid ledger was marshaled")
	}
}

func TestLedgerV2AdvanceAppliesExactlyNextMigration(t *testing.T) {
	registry, _, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	ledger := BaselineLedgerV2(registry)
	advanced, err := ledger.Advance(registry, registry.Migrations[0])
	if err != nil {
		t.Fatal(err)
	}
	state := advanced.Domains["settings"]
	if state.Epoch != 2 || !slices.Equal(state.Applied, []string{"settings-v2"}) {
		t.Fatalf("advanced ledger = %#v", advanced)
	}
	if _, err := advanced.Advance(registry, registry.Migrations[0]); err == nil {
		t.Fatal("already-applied migration advanced twice")
	}
	if _, err := ledger.Advance(registry, registry.Migrations[1]); err == nil {
		t.Fatal("out-of-order migration advanced")
	}
}
