package releasetransition

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

const validRegistryV2 = `{
  "schemaVersion": 2,
  "minimumEpochs": {"settings": 1, "project-state": 1},
  "currentEpochs": {"settings": 3, "project-state": 2},
  "migrations": [
    {"id":"settings-v2","domain":"settings","fromEpoch":1,"toEpoch":2,"kind":"test-vms-settings-v1-to-v2"},
    {"id":"project-v2","domain":"project-state","fromEpoch":1,"toEpoch":2,"kind":"project-state-fixture-v1-to-v2","dependsOn":["settings-v2"]},
    {"id":"settings-v3","domain":"settings","fromEpoch":2,"toEpoch":3,"kind":"settings-fixture-v2-to-v3","dependsOn":["project-v2"]}
  ]
}`

func TestRegistryV2CanonicalDigestBindsEveryField(t *testing.T) {
	registry, _, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	registry.RetiredPrefixes = map[string]RetiredMigrationPrefixV2{
		"settings": {ThroughEpoch: 1, Digest: Fingerprint(strings.Repeat("a", 64))},
	}
	digest, err := registry.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*RegistryV2){
		"schema":        func(r *RegistryV2) { r.SchemaVersion++ },
		"minimum epoch": func(r *RegistryV2) { r.MinimumEpochs["settings"]++ },
		"maximum epoch": func(r *RegistryV2) { r.CurrentEpochs["settings"]++ },
		"migration ID":  func(r *RegistryV2) { r.Migrations[0].ID = "different" },
		"kind":          func(r *RegistryV2) { r.Migrations[0].Kind = "different" },
		"domain":        func(r *RegistryV2) { r.Migrations[0].Domain = "project-state" },
		"from epoch":    func(r *RegistryV2) { r.Migrations[0].FromEpoch++ },
		"to epoch":      func(r *RegistryV2) { r.Migrations[0].ToEpoch++ },
		"dependency":    func(r *RegistryV2) { r.Migrations[1].DependsOn[0] = "different" },
		"retired epoch": func(r *RegistryV2) {
			r.RetiredPrefixes["settings"] = RetiredMigrationPrefixV2{ThroughEpoch: 2, Digest: registry.RetiredPrefixes["settings"].Digest}
		},
		"retired digest": func(r *RegistryV2) {
			r.RetiredPrefixes["settings"] = RetiredMigrationPrefixV2{ThroughEpoch: 1, Digest: Fingerprint(strings.Repeat("b", 64))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var changed RegistryV2
			if err := json.Unmarshal(payload, &changed); err != nil {
				t.Fatal(err)
			}
			change(&changed)
			got, err := changed.CanonicalDigest()
			if err != nil || got == digest {
				t.Fatalf("field change did not change digest: %s, %v", got, err)
			}
		})
	}
}

func TestParseRegistryV2ValidatesAndReturnsExactBytesDigest(t *testing.T) {
	catalog := registryV2TestCatalog(t)
	registry, digest, err := ParseRegistryV2([]byte(validRegistryV2), catalog)
	if err != nil {
		t.Fatal(err)
	}
	if digest != fingerprintPayload([]byte(validRegistryV2)) {
		t.Fatalf("digest = %q", digest)
	}
	path, err := registry.Path("settings", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{path[0].ID, path[1].ID}; !slices.Equal(got, []string{"settings-v2", "settings-v3"}) {
		t.Fatalf("settings path = %v", got)
	}
	if _, err := registry.Path("settings", 3); err != nil {
		t.Fatalf("current epoch path: %v", err)
	}
}

func TestParseRegistryV2RejectsUnboundedUnknownAndTrailingJSON(t *testing.T) {
	catalog := registryV2TestCatalog(t)
	for name, payload := range map[string]string{
		"empty":         "",
		"unbounded":     strings.Repeat("x", MaxRegistryV2Bytes+1),
		"unknown field": strings.Replace(validRegistryV2, `"schemaVersion": 2,`, `"schemaVersion": 2, "effect":"shell",`, 1),
		"trailing":      validRegistryV2 + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseRegistryV2([]byte(payload), catalog); err == nil {
				t.Fatal("invalid registry was accepted")
			}
		})
	}
}

func TestRegistryV2RejectsAmbiguousDomainGraphsAndUnknownKinds(t *testing.T) {
	catalog := registryV2TestCatalog(t)
	tests := map[string]string{
		"schema":             strings.Replace(validRegistryV2, `"schemaVersion": 2`, `"schemaVersion": 1`, 1),
		"duplicate id":       strings.Replace(validRegistryV2, `"id":"project-v2"`, `"id":"settings-v2"`, 1),
		"unknown domain":     strings.Replace(validRegistryV2, `"domain":"project-state"`, `"domain":"foreign"`, 1),
		"unknown kind":       strings.Replace(validRegistryV2, `"kind":"settings-fixture-v2-to-v3"`, `"kind":"shell"`, 1),
		"gap":                strings.Replace(validRegistryV2, `"fromEpoch":2,"toEpoch":3`, `"fromEpoch":3,"toEpoch":4`, 1),
		"overlap":            strings.Replace(validRegistryV2, `"fromEpoch":2,"toEpoch":3`, `"fromEpoch":1,"toEpoch":2`, 1),
		"not one epoch":      strings.Replace(validRegistryV2, `"fromEpoch":2,"toEpoch":3`, `"fromEpoch":2,"toEpoch":4`, 1),
		"missing dependency": strings.Replace(validRegistryV2, `"dependsOn":["project-v2"]`, `"dependsOn":["missing"]`, 1),
		"forward dependency": strings.Replace(
			strings.Replace(validRegistryV2, `"kind":"test-vms-settings-v1-to-v2"`, `"kind":"test-vms-settings-v1-to-v2","dependsOn":["project-v2"]`, 1),
			`,"dependsOn":["settings-v2"]`, "", 1,
		),
		"cycle":                strings.Replace(validRegistryV2, `"kind":"test-vms-settings-v1-to-v2"`, `"kind":"test-vms-settings-v1-to-v2","dependsOn":["project-v2"]`, 1),
		"duplicate dependency": strings.Replace(validRegistryV2, `"dependsOn":["project-v2"]`, `"dependsOn":["project-v2","project-v2"]`, 1),
		"current mismatch":     strings.Replace(validRegistryV2, `"settings": 3`, `"settings": 4`, 1),
		"domain map mismatch":  strings.Replace(validRegistryV2, `"currentEpochs": {`, `"currentEpochs": {"foreign":1,`, 1),
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseRegistryV2([]byte(payload), catalog); err == nil {
				t.Fatal("invalid registry was accepted")
			}
		})
	}
}

func TestRegistryV2PathRejectsUnknownUnsupportedAndFutureEpochs(t *testing.T) {
	registry, _, err := ParseRegistryV2([]byte(validRegistryV2), registryV2TestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		domain string
		epoch  int
	}{
		"unknown": {"foreign", 1},
		"old":     {"settings", 0},
		"future":  {"settings", 4},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := registry.Path(test.domain, test.epoch); err == nil {
				t.Fatal("unsupported path was accepted")
			}
		})
	}
}

func TestRegistryV2RetirementPreservesPrefixDigestAcrossRemainingSuffix(t *testing.T) {
	catalog := registryV2TestCatalog(t)
	original, _, err := ParseRegistryV2([]byte(pinnedCheckpointRegistryV2Payload), catalog)
	if err != nil {
		t.Fatal(err)
	}
	for epoch, want := range map[int]Fingerprint{2: pinnedSettingsV2PrefixDigest, 3: pinnedSettingsV3PrefixDigest} {
		if got, err := original.MigrationPrefixDigest("settings", epoch); err != nil || got != want {
			t.Fatalf("historical prefix at epoch %d = %q, want %q, err=%v", epoch, got, want, err)
		}
	}
	retired := original
	retired.MinimumEpochs = cloneEpochs(original.MinimumEpochs)
	retired.MinimumEpochs["settings"] = 2
	retired.Migrations = append([]MigrationDefinitionV2(nil), original.Migrations[1:]...)
	retired.RetiredPrefixes = map[string]RetiredMigrationPrefixV2{
		"settings": {ThroughEpoch: 2, Digest: pinnedSettingsV2PrefixDigest},
	}
	if err := retired.Validate(catalog); err != nil {
		t.Fatalf("retired registry invalid: %v", err)
	}
	got, err := retired.MigrationPrefixDigest("settings", 3)
	if err != nil || got != pinnedSettingsV3PrefixDigest {
		t.Fatalf("prefix digest after retirement = %q, want %q, err=%v", got, pinnedSettingsV3PrefixDigest, err)
	}
}

func TestCapabilityCatalogDigestIsCanonicalAndRejectsDuplicates(t *testing.T) {
	left, err := NewCapabilityCatalog([]CapabilityDescriptor{
		{Kind: "b-v1", Domain: "settings", Version: 1},
		{Kind: "a-v1", Domain: "settings", Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewCapabilityCatalog([]CapabilityDescriptor{
		{Kind: "a-v1", Domain: "settings", Version: 1},
		{Kind: "b-v1", Domain: "settings", Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if left.Digest() != right.Digest() {
		t.Fatalf("catalog digest depends on input order: %q != %q", left.Digest(), right.Digest())
	}
	if _, err := NewCapabilityCatalog([]CapabilityDescriptor{
		{Kind: "a-v1", Domain: "settings", Version: 1},
		{Kind: "a-v1", Domain: "settings", Version: 1},
	}); err == nil {
		t.Fatal("duplicate catalog kind was accepted")
	}
	if _, err := NewCapabilityCatalog([]CapabilityDescriptor{{Kind: "bad", Domain: "settings", Version: 0}}); err == nil {
		t.Fatal("invalid catalog version was accepted")
	}
	if _, err := NewCapabilityCatalog([]CapabilityDescriptor{{
		Kind: "bad", Domain: "settings", Version: 1, RollbackCompatibility: "unknown",
	}}); err == nil {
		t.Fatal("unknown compiled rollback policy was accepted")
	}
}

func TestCapabilityCatalogDigestBindsCompiledImplementationVersion(t *testing.T) {
	base := CapabilityDescriptor{Kind: "settings-v1", Domain: "settings", Version: 1}
	baseline, err := NewCapabilityCatalog([]CapabilityDescriptor{base})
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Version++
	catalog, err := NewCapabilityCatalog([]CapabilityDescriptor{changed})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Digest() == baseline.Digest() {
		t.Fatal("catalog digest did not bind the compiled implementation version")
	}
}

func TestCapabilityCatalogDigestBindsCompiledRollbackPolicy(t *testing.T) {
	base := CapabilityDescriptor{
		Kind: "settings-v1", Domain: "settings", Version: 1,
		RollbackCompatibility: RollbackCompatibilityBackwardCompatibleNoOp,
		LegacyMinimumVersion:  "0.8.0",
	}
	baseline, err := NewCapabilityCatalog([]CapabilityDescriptor{base})
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*CapabilityDescriptor){
		"policy": func(value *CapabilityDescriptor) {
			value.RollbackCompatibility = ""
			value.LegacyMinimumVersion = ""
		},
		"legacy minimum": func(value *CapabilityDescriptor) { value.LegacyMinimumVersion = "0.9.0" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			change(&changed)
			catalog, err := NewCapabilityCatalog([]CapabilityDescriptor{changed})
			if err != nil {
				t.Fatal(err)
			}
			if catalog.Digest() == baseline.Digest() {
				t.Fatalf("catalog digest did not bind compiled rollback %s", name)
			}
		})
	}
}

func registryV2TestCatalog(t *testing.T) CapabilityCatalog {
	t.Helper()
	catalog, err := NewCapabilityCatalog([]CapabilityDescriptor{
		{Kind: "test-vms-settings-v1-to-v2", Domain: "settings", Version: 1},
		{Kind: "project-state-fixture-v1-to-v2", Domain: "project-state", Version: 1},
		{Kind: "settings-fixture-v2-to-v3", Domain: "settings", Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestBuiltinCapabilityCatalogContainsPublishedV2Kinds(t *testing.T) {
	catalog := BuiltinCapabilityCatalog()
	if !catalog.Supports("test-vms-settings-v1-to-v2", "settings") {
		t.Fatal("published settings capability is missing")
	}
	if !catalog.Supports("test-yard-owner-v1-to-v2", "owner-registration") {
		t.Fatal("published owner-registration capability is missing")
	}
	if catalog.Supports("shell", "settings") || catalog.Supports("test-vms-settings-v1-to-v2", "foreign") {
		t.Fatal("catalog accepted a foreign capability")
	}
	if !validFingerprint(catalog.Digest()) {
		t.Fatalf("catalog digest = %q", catalog.Digest())
	}
}

func TestPublishedReleaseTransitionRegistryMatchesBuiltinCatalog(t *testing.T) {
	payload, err := os.ReadFile("../../config/release-transition.json")
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Migrations) != 1 || registry.Migrations[0].Kind != nmDeviceGuardMigration || digest == historyV028Digest {
		t.Fatalf("live registry = %#v", registry.Migrations)
	}
	if registry.CurrentEpochs["power-metadata"] != 4 {
		t.Fatalf("published power reader boundary = %#v", registry.CurrentEpochs)
	}
	for domain, top := range historyV028.CurrentEpochs {
		if registry.MinimumEpochs[domain] != top || domain != "power-metadata" && registry.CurrentEpochs[domain] != top {
			t.Fatalf("wrong floor: %s", domain)
		}
		if top == 1 {
			continue
		}
		prefix, err := historyV028.MigrationPrefixDigest(domain, top)
		if err != nil || registry.RetiredPrefixes[domain].ThroughEpoch != top || registry.RetiredPrefixes[domain].Digest != prefix {
			t.Fatalf("changed historical prefix: %s %v", domain, err)
		}
	}
}

func TestRegistryV2ErrorsRemainClassifiableAsInvalid(t *testing.T) {
	for _, payload := range []string{`{}`, `{"schemaVersion":2,"unknown":true}`} {
		_, _, err := ParseRegistryV2([]byte(payload), registryV2TestCatalog(t))
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
		if !errors.Is(err, ErrRegistryInvalid) {
			t.Fatalf("error = %v, want ErrRegistryInvalid", err)
		}
	}
}

// Released power readers reject the [device-subyard] NetworkManager guard and their init rewrites
// the released guard, so a retained release older than this boundary is not a rollback target.
func TestNetworkManagerDeviceGuardForbidsRollbackToReleasedReaders(t *testing.T) {
	descriptor, ok := BuiltinCapabilityCatalog().descriptor(nmDeviceGuardMigration)
	if !ok || descriptor.Domain != "power-metadata" || descriptor.RollbackCompatibility != "" {
		t.Fatalf("NetworkManager device guard descriptor = %#v, %v", descriptor, ok)
	}
	const publishedSixKindDigest Fingerprint = "ed0b321ebbdcc81f039d6d25ff67800736be13af18b048ba837ea79fae8ccd3c"
	catalog, supported := SupportedBuiltinCatalog(publishedSixKindDigest)
	if !supported || len(catalog.Descriptors()) != 6 {
		t.Fatal("journals sealed by the previous catalog are no longer recognized")
	}
}
