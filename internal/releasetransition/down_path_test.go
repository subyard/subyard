package releasetransition

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishedV030Registry is the exact v0.30.0 tag registry
// (testdata/v0.30.0-release-transition.json, sha256 3ee06045...).
func publishedV030Registry(t *testing.T) RegistryV2 {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", "v0.30.0-release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if err != nil || digest != "3ee060450263c840853ecb639200a6e4a02b52c7a70e7e8c5d35eade99091de2" {
		t.Fatalf("v0.30.0 registry %s: %v", digest, err)
	}
	return registry
}

// retiringCandidateRegistry retires every completed v0.30.0 prefix except the
// newest power-metadata step. Its later definition keeps its dependency on
// retired history, which this engine's registry validation does not accept, so
// it is decoded without validation.
const retiringCandidateRegistry = `{"schemaVersion":2,` +
	`"minimumEpochs":{"owner-registration":3,"power-metadata":3,"project-state":1,"settings":3},` +
	`"currentEpochs":{"owner-registration":3,"power-metadata":4,"project-state":1,"settings":3},` +
	`"migrations":[{"id":"introduce-nm-device-guard-v4","domain":"power-metadata","fromEpoch":3,"toEpoch":4,"kind":"nm-device-guard-v1","dependsOn":["complete-registered-guest-disk-tmp-v3"]}],` +
	`"retiredPrefixes":{` +
	`"owner-registration":{"throughEpoch":3,"digest":"f612c2a88cfb6a2f1f3194acf13b933d3009d8853c9f093e07e5cad491c1aab6"},` +
	`"power-metadata":{"throughEpoch":3,"digest":"7692dc70c24e437966ba441be41204ec2642f3ceda004ef3492dd10c6826467d"},` +
	`"settings":{"throughEpoch":3,"digest":"559acd8e21b67b9f4516f44ec2c67f655898a9094147d5686c3816990c47855e"}}}`

func decodeRetiringCandidate(t *testing.T) RegistryV2 {
	t.Helper()
	var registry RegistryV2
	if err := json.Unmarshal([]byte(retiringCandidateRegistry), &registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestRetirePrefixesReproducesPublishedPrefixDigests(t *testing.T) {
	v030, candidate := publishedV030Registry(t), decodeRetiringCandidate(t)
	floors := map[string]int{"owner-registration": 3, "power-metadata": 3, "settings": 3}
	retired, err := v030.RetirePrefixes(floors)
	if err != nil {
		t.Fatal(err)
	}
	for domain, floor := range floors {
		proof, err := v030.MigrationPrefixDigest(domain, floor)
		if err != nil || proof != candidate.RetiredPrefixes[domain].Digest || retired.RetiredPrefixes[domain].Digest != proof {
			t.Fatalf("%s retired digest %s, v0.30.0 prefix %s, %v", domain, candidate.RetiredPrefixes[domain].Digest, proof, err)
		}
	}
	got, err := retired.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	want, err := candidate.CanonicalDigest()
	if err != nil || got != want {
		t.Fatalf("v0.30.0 retired through its floors = %s, candidate %s, %v", got, want, err)
	}
	if len(v030.RetiredPrefixes) != 0 || v030.MinimumEpochs["settings"] != 1 || len(v030.Migrations) != 7 {
		t.Fatal("retirement changed its source registry")
	}
}

// singleDomainRegistry has one settings path 1 -> 2 -> 3 without dependencies,
// so its retired forms remain valid registries.
func singleDomainRegistry() RegistryV2 {
	return RegistryV2{
		SchemaVersion: RegistrySchemaV2,
		MinimumEpochs: map[string]int{"settings": 1}, CurrentEpochs: map[string]int{"settings": 3},
		Migrations: []MigrationDefinitionV2{
			{ID: "settings-v2", Domain: "settings", FromEpoch: 1, ToEpoch: 2, Kind: "test-vms-settings-v1-to-v2"},
			{ID: "settings-v3", Domain: "settings", FromEpoch: 2, ToEpoch: 3, Kind: "local-ssh-port-settings-v2-to-v3"},
		},
	}
}

func TestRetirePrefixesChainsLaterHistoryAndRejectsFloorsOutsideTheRegistry(t *testing.T) {
	full := singleDomainRegistry()
	retired, err := full.RetirePrefixes(map[string]int{"settings": 2})
	if err != nil {
		t.Fatal(err)
	}
	if retired.MinimumEpochs["settings"] != 2 || len(retired.Migrations) != 1 || retired.Migrations[0].ID != "settings-v3" {
		t.Fatalf("retired registry = %#v", retired)
	}
	for epoch := 2; epoch <= 3; epoch++ {
		before, err := full.MigrationPrefixDigest("settings", epoch)
		if err != nil {
			t.Fatal(err)
		}
		after, err := retired.MigrationPrefixDigest("settings", epoch)
		if err != nil || after != before {
			t.Fatalf("prefix through %d = %s after retirement, %s before, %v", epoch, after, before, err)
		}
	}
	repeated, err := retired.RetirePrefixes(map[string]int{"settings": 2})
	if err != nil {
		t.Fatal(err)
	}
	further, err := retired.RetirePrefixes(map[string]int{"settings": 3})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := full.RetirePrefixes(map[string]int{"settings": 3})
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]RegistryV2{"repeated floor": {retired, repeated}, "stepwise": {further, direct}} {
		first, _ := pair[0].CanonicalDigest()
		second, _ := pair[1].CanonicalDigest()
		if first != second {
			t.Errorf("%s retirement differs", name)
		}
	}
	for name, test := range map[string]struct {
		registry RegistryV2
		floors   map[string]int
	}{
		"unknown domain":       {full, map[string]int{"unknown": 1}},
		"beyond current":       {full, map[string]int{"settings": 4}},
		"below registry floor": {full, map[string]int{"settings": 0}},
		"below retired floor":  {retired, map[string]int{"settings": 1}},
	} {
		if _, err := test.registry.RetirePrefixes(test.floors); err == nil {
			t.Errorf("%s: retirement succeeded", name)
		}
	}
	if len(full.RetiredPrefixes) != 0 || full.MinimumEpochs["settings"] != 1 || len(full.Migrations) != 2 {
		t.Fatal("retirement changed its source registry")
	}
}

// The engine compares its own and the retained target's parsed registries by
// canonical digest, like the runtime boundary. Only identical sealed bytes can
// stand in for a missing target registry.
func TestV2RollbackRegistryComparesParsedRegistries(t *testing.T) {
	own := fingerprintPayload(v2RegistryPayload(t))
	reformatted := v2TestRegistry(t)
	older := olderHistoryRegistry(reformatted)
	other := Fingerprint(strings.Repeat("c", 64))
	for _, test := range []struct {
		name     string
		digest   Fingerprint
		registry *RegistryV2
		refused  bool
	}{
		{name: "identical sealed bytes", digest: own},
		{name: "differing bytes without a parsed registry", digest: other, refused: true},
		{name: "canonical identity", digest: other, registry: &reformatted},
		{name: "differing registry", digest: other, registry: &older, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newV2RollbackBoundaryFixture(t, "0.30.0", test.digest, nil)
			fixture.transition.options.RollbackTargetRegistry = test.registry
			if refused := fixture.transition.rollbackRegistryRefused(); refused != test.refused {
				t.Fatalf("rollback registry refused = %v", refused)
			}
		})
	}
	options := newV2RollbackBoundaryFixture(t, "0.30.0", "", nil).transition.options
	options.RollbackTargetRegistry = &reformatted
	if _, err := NewV2Transition(options); err == nil {
		t.Fatal("a registryless rollback target accepted a target registry")
	}
}
