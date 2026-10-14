package releasetransition

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSourceMetadataCatalogPreservesPublishedSemantics(t *testing.T) {
	catalog := BuiltinCapabilityCatalog()
	historical, err := NewCapabilityCatalog([]CapabilityDescriptor{
		{Kind: "test-vms-settings-v1-to-v2", Domain: "settings", Version: 1,
			RollbackCompatibility: RollbackCompatibilityBackwardCompatibleNoOp, LegacyMinimumVersion: "0.8.0"},
		{Kind: "test-yard-owner-v1-to-v2", Domain: "owner-registration", Version: 1,
			RollbackCompatibility: RollbackCompatibilityBackwardCompatibleNoOp, LegacyMinimumVersion: "0.8.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := ParseRegistryV2([]byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":2},"migrations":[{"id":"settings-v2","domain":"settings","fromEpoch":1,"toEpoch":2,"kind":"test-vms-settings-v1-to-v2"}]}`), catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.SupportsSourceMetadata(historical.Digest(), registry) || !catalog.SupportsSourceMetadata(catalog.Digest(), registry) {
		t.Fatal("supported exact catalog refused read-only historical metadata")
	}
	for _, kind := range []string{localSSHPortMigration, "owner-publication-state-v1"} {
		descriptor, _ := catalog.descriptor(kind)
		retained, err := NewCapabilityCatalog(append(slices.Clone(historical.descriptors), descriptor))
		if err != nil {
			t.Fatal(err)
		}
		if !catalog.SupportsSourceMetadata(retained.Digest(), registry) {
			t.Fatalf("independent %s catalog refused read-only historical metadata", kind)
		}
	}
	if catalog.SupportsSourceMetadata(Fingerprint(strings.Repeat("c", 64)), registry) {
		t.Fatal("unknown original catalog admitted")
	}
	descriptors := slices.Clone(catalog.descriptors)
	for index := range descriptors {
		if descriptors[index].Kind == "test-vms-settings-v1-to-v2" {
			descriptors[index].Version++
		}
	}
	changed, err := NewCapabilityCatalog(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SupportsSourceMetadata(historical.Digest(), registry) {
		t.Fatal("changed compiled semantics admitted an earlier catalog")
	}
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	publication, _, err := ParseRegistryV2(payload, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.SupportsSourceMetadata(catalog.Digest(), publication) || catalog.SupportsSourceMetadata(historical.Digest(), publication) {
		t.Fatal("publication registry escaped its exact compiled catalog boundary")
	}
}
