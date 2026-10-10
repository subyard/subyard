package releasetransition

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// These compatibility vectors pin schema 1 bytes and SHA-256 prefix proofs.
// Keep them literal: generating them with today's writer would hide format drift.
const (
	pinnedSettingsV2PrefixDigest        Fingerprint = "4f797a7d2346b237e0ded078391c8b5c6e5de0779125538b320f91b4ac2cec13"
	pinnedSettingsV3PrefixDigest        Fingerprint = "6aa9ec03951772bfa196bf19e605e97d97b70b0cffa24d5909b26a8a5ad5daf7"
	pinnedCheckpointRegistryV2Payload               = `{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":3},"migrations":[{"id":"settings-v2","domain":"settings","fromEpoch":1,"toEpoch":2,"kind":"test-vms-settings-v1-to-v2"},{"id":"settings-v3","domain":"settings","fromEpoch":2,"toEpoch":3,"kind":"settings-fixture-v2-to-v3"}]}` + "\n"
	pinnedCheckpointProjectionV2Payload             = `{"schemaVersion":2,"domains":{"settings":{"epoch":2,"applied":["settings-v2"]}}}` + "\n"
	pinnedLedgerCheckpointV1Payload                 = `{"schemaVersion":1,"registryDigest":"1f947ac1497631dc5fb100d8492595c1a63ebf7418e8230329110d518dc26b74","legacyProjection":{"exists":true,"fingerprint":"2d463e50a574030827f91f71e24a07db2339e9e84f6ba84e4c304cdd0894c105","epochs":{"settings":2}},"domains":{"settings":{"epoch":2,"compactedThrough":2,"prefixDigest":"4f797a7d2346b237e0ded078391c8b5c6e5de0779125538b320f91b4ac2cec13","appliedSuffix":[]}}}` + "\n"
)

func TestLedgerCheckpointV1ReadsAndAdvancesPinnedHistoryAfterRetirement(t *testing.T) {
	registry, digest, err := ParseRegistryV2([]byte(pinnedCheckpointRegistryV2Payload), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	projection := protectedSnapshotFromPayload([]byte(pinnedCheckpointProjectionV2Payload))
	checkpoint := protectedSnapshotFromPayload([]byte(pinnedLedgerCheckpointV1Payload))
	historical, err := ParseMigrationLedger(projection, checkpoint, registry, digest)
	if err != nil {
		t.Fatalf("read historical checkpoint: %v", err)
	}
	payload, _, err := MarshalLedgerCheckpointV1(historical.Checkpoint, registry, digest, projection)
	if err != nil || string(payload) != pinnedLedgerCheckpointV1Payload {
		t.Fatalf("historical checkpoint bytes changed: %s, err=%v", payload, err)
	}

	registry.MinimumEpochs["settings"] = 2
	registry.Migrations = registry.Migrations[1:]
	registry.RetiredPrefixes = map[string]RetiredMigrationPrefixV2{
		"settings": {ThroughEpoch: 2, Digest: pinnedSettingsV2PrefixDigest},
	}
	if err := registry.Validate(registryV2TestCatalog(t)); err != nil {
		t.Fatal(err)
	}
	digest = fingerprintPayload([]byte("retired registry"))
	actual, err := ParseMigrationLedger(projection, checkpoint, registry, digest)
	if err != nil {
		t.Fatalf("read historical checkpoint after retirement: %v", err)
	}
	pending, err := registry.PendingPath(actual.Ledger)
	if err != nil || len(pending) != 1 || pending[0].ID != "settings-v3" {
		t.Fatalf("pending path after retirement = %#v, err=%v", pending, err)
	}
	next, err := actual.Ledger.Advance(registry, pending[0])
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := advanceLedgerCheckpointV1(actual.Checkpoint, next, registry, digest, projection)
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err = MarshalLedgerCheckpointV1(advanced, registry, digest, projection)
	if err != nil {
		t.Fatal(err)
	}
	actual, err = ParseMigrationLedger(projection, protectedSnapshotFromPayload(payload), registry, digest)
	if err != nil {
		t.Fatalf("read advanced historical checkpoint: %v", err)
	}
	state := actual.Checkpoint.Domains["settings"]
	if actual.Ledger.Domains["settings"].Epoch != 3 || state.CompactedThrough != 2 ||
		state.PrefixDigest != pinnedSettingsV2PrefixDigest || len(state.AppliedSuffix) != 1 || state.AppliedSuffix[0] != "settings-v3" ||
		!bytes.Equal(actual.ProjectionSnapshot.Payload, projection.Payload) {
		t.Fatalf("advanced historical checkpoint lost prefix, suffix, or projection: %#v", actual)
	}
}

func TestLedgerCheckpointV1AllowsCompatibleRegistrySuffixGrowth(t *testing.T) {
	registry, digest, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	ledger := LedgerV2{SchemaVersion: LedgerSchemaV2, Domains: map[string]DomainLedgerV2{
		"settings":      {Epoch: 2, Applied: []string{"settings-v2"}},
		"project-state": {Epoch: 2, Applied: []string{"project-v2"}},
	}}
	projectionPayload, _, err := MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	projection := protectedSnapshotFromPayload(projectionPayload)
	checkpoint, err := newLedgerCheckpointV1(
		ledger, registry, digest, projection,
		map[string]int{"settings": 2, "project-state": 2},
	)
	if err != nil {
		t.Fatal(err)
	}

	// A later trusted registry may append work beyond the compacted prefix.
	// The checkpoint digest remains provenance; the prefix digest proves the
	// completed history and the new suffix stays pending.
	extended := registry
	extended.CurrentEpochs = cloneEpochs(registry.CurrentEpochs)
	extended.CurrentEpochs["settings"] = 4
	extended.Migrations = append(append([]MigrationDefinitionV2(nil), registry.Migrations...),
		MigrationDefinitionV2{
			ID: "settings-v4", Domain: "settings", FromEpoch: 3, ToEpoch: 4,
			Kind: "settings-fixture-v2-to-v3",
		})
	newDigest := fingerprintPayload([]byte("new registry bytes"))
	if err := checkpoint.Validate(extended, newDigest, projection); err != nil {
		t.Fatalf("compatible registry extension rejected: %v", err)
	}
	view, err := checkpoint.LedgerView(extended)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := extended.PendingPath(view)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != "settings-v3" {
		t.Fatalf("pending path repeated completed work or lost suffix: %#v", pending)
	}
	advanced, err := view.Advance(extended, pending[0])
	if err != nil {
		t.Fatal(err)
	}
	next, err := advanceLedgerCheckpointV1(checkpoint, advanced, extended, newDigest, projection)
	if err != nil {
		t.Fatal(err)
	}
	if got := next.Domains["settings"]; got.Epoch != 3 || got.CompactedThrough != 2 ||
		len(got.AppliedSuffix) != 1 || got.AppliedSuffix[0] != "settings-v3" {
		t.Fatalf("checkpoint suffix after advance = %#v", got)
	}
	view, err = next.LedgerView(extended)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = extended.PendingPath(view)
	if err != nil || len(pending) != 1 || pending[0].ID != "settings-v4" {
		t.Fatalf("next pending path = %#v, %v", pending, err)
	}
}

func TestLedgerCheckpointV1RejectsChangedCompletedHistoryAndProjectionEpochTampering(t *testing.T) {
	registry, digest, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	ledger := LedgerV2{SchemaVersion: LedgerSchemaV2, Domains: map[string]DomainLedgerV2{
		"settings":      {Epoch: 2, Applied: []string{"settings-v2"}},
		"project-state": {Epoch: 1, Applied: []string{}},
	}}
	projectionPayload, _, err := MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	projection := protectedSnapshotFromPayload(projectionPayload)
	checkpoint, err := newLedgerCheckpointV1(ledger, registry, digest, projection,
		map[string]int{"settings": 2, "project-state": 1})
	if err != nil {
		t.Fatal(err)
	}
	changed := registry
	changed.Migrations = append([]MigrationDefinitionV2(nil), registry.Migrations...)
	changed.Migrations[0].Kind = "other-kind"
	if err := checkpoint.Validate(changed, fingerprintPayload([]byte("changed registry")), projection); err == nil {
		t.Fatal("changed completed migration definition was accepted")
	}

	checkpoint.LegacyProjection.Epochs["settings"] = 1
	if err := checkpoint.Validate(registry, digest, projection); err == nil {
		t.Fatal("ledger epoch map inconsistent with pinned projection bytes was accepted")
	}
}

func TestLedgerCheckpointV1RejectsRemovedDomain(t *testing.T) {
	registry, digest, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	ledger := BaselineLedgerV2(registry)
	projectionPayload, _, err := MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	projection := protectedSnapshotFromPayload(projectionPayload)
	checkpoint, err := newLedgerCheckpointV1(ledger, registry, digest, projection,
		map[string]int{"settings": 1, "project-state": 1})
	if err != nil {
		t.Fatal(err)
	}
	removed := registry
	removed.MinimumEpochs = map[string]int{"settings": 1}
	removed.CurrentEpochs = map[string]int{"settings": 3}
	removed.Migrations = append([]MigrationDefinitionV2(nil), registry.Migrations[:1]...)
	if err := checkpoint.Validate(removed, fingerprintPayload([]byte("removed domain")), projection); err == nil {
		t.Fatal("checkpoint for removed domain was accepted")
	}
}

func TestLedgerCheckpointV1CanBindAbsentLegacyProjectionBaseline(t *testing.T) {
	registry, digest, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	projection := absentProtectedSnapshot()
	ledger := BaselineLedgerV2(registry)
	epochs := make(map[string]int, len(ledger.Domains))
	for domain, state := range ledger.Domains {
		epochs[domain] = state.Epoch
	}
	checkpoint, err := newLedgerCheckpointV1(ledger, registry, digest, projection, epochs)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.LegacyProjection.Exists || checkpoint.LegacyProjection.Fingerprint != "" {
		t.Fatalf("absent projection binding = %#v", checkpoint.LegacyProjection)
	}
	if _, _, err := MarshalLedgerCheckpointV1(checkpoint, registry, digest, projection); err != nil {
		t.Fatalf("absent projection checkpoint did not validate: %v", err)
	}
}

func TestMigrationLedgerCheckpointAuthoritySurvivesFutureSuffixAdvance(t *testing.T) {
	registry, digest, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	configHome := protectedConfigHome(t)
	store, err := NewPOSIXV2Store(configHome)
	if err != nil {
		t.Fatal(err)
	}
	ledger := LedgerV2{SchemaVersion: LedgerSchemaV2, Domains: map[string]DomainLedgerV2{
		"settings":      {Epoch: 2, Applied: []string{"settings-v2"}},
		"project-state": {Epoch: 2, Applied: []string{"project-v2"}},
	}}
	payload, _, err := MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(missing, payload); err != nil {
		t.Fatal(err)
	}
	expected, err := store.ReadMigrationLedger(registry, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConvertMigrationLedger(expected, registry, digest, registry, digest); err != nil {
		t.Fatal(err)
	}
	legacyBefore, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyBefore.Payload, payload) {
		t.Fatal("checkpoint conversion rewrote the legacy projection")
	}

	extended := registry
	extended.CurrentEpochs = cloneEpochs(registry.CurrentEpochs)
	extended.CurrentEpochs["settings"] = 4
	extended.Migrations = append(append([]MigrationDefinitionV2(nil), registry.Migrations...),
		MigrationDefinitionV2{
			ID: "settings-v4", Domain: "settings", FromEpoch: 3, ToEpoch: 4,
			Kind: "settings-fixture-v2-to-v3",
		})
	newDigest := fingerprintPayload([]byte("extended registry"))
	actual, err := store.ReadMigrationLedger(extended, newDigest)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := extended.PendingPath(actual.Ledger)
	if err != nil || len(pending) != 2 || pending[0].ID != "settings-v3" {
		t.Fatalf("extended pending path = %#v, %v", pending, err)
	}
	next, err := actual.Ledger.Advance(extended, pending[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapMigrationLedger(actual, next, extended, newDigest); err != nil {
		t.Fatal(err)
	}
	actual, err = store.ReadMigrationLedger(extended, newDigest)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = extended.PendingPath(actual.Ledger)
	if err != nil || len(pending) != 1 || pending[0].ID != "settings-v4" {
		t.Fatalf("post-advance pending path = %#v, %v", pending, err)
	}
	if err := store.ConvertMigrationLedger(actual, extended, newDigest, extended, newDigest); err != nil {
		t.Fatalf("ready checkpoint compaction: %v", err)
	}
	actual, err = store.ReadMigrationLedger(extended, newDigest)
	if err != nil || ledgerCheckpointHasSuffix(actual.Checkpoint) ||
		actual.Checkpoint.Domains["settings"].CompactedThrough != 3 {
		t.Fatalf("ready checkpoint retained completed suffix: %#v err=%v", actual.Checkpoint, err)
	}
	legacyAfter, err := store.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyAfter.Payload, payload) {
		t.Fatal("checkpoint suffix advance rewrote the legacy projection")
	}
	if _, err := os.Stat(filepath.Join(configHome, "release-transition", "v2", migrationLedgerCheckpointName)); err != nil {
		t.Fatal(err)
	}
}
