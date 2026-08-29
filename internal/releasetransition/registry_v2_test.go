package releasetransition

import (
	"errors"
	"os"
	"path/filepath"
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
		"forward dependency": strings.Replace(validRegistryV2, `"kind":"test-vms-settings-v1-to-v2"`, `"kind":"test-vms-settings-v1-to-v2","dependsOn":["project-v2"]`, 1),
		"cycle": strings.Replace(
			strings.Replace(validRegistryV2, `"kind":"test-vms-settings-v1-to-v2"`, `"kind":"test-vms-settings-v1-to-v2","dependsOn":["project-v2"]`, 1),
			`"dependsOn":["settings-v2"]`, `"dependsOn":["settings-v2"]`, 1,
		),
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
	payload, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if registry.MinimumEpochs["settings"] != 1 || registry.CurrentEpochs["settings"] != 2 ||
		registry.MinimumEpochs["owner-registration"] != 1 ||
		registry.CurrentEpochs["owner-registration"] != 2 || len(registry.Migrations) != 2 ||
		registry.Migrations[0].ID != "canonicalize-test-vms-settings-v2" ||
		registry.Migrations[1].ID != "canonicalize-test-yard-owner-v2" {
		t.Fatalf("published registry = %#v", registry)
	}
	for _, domain := range []string{"power-metadata", "project-state"} {
		if registry.MinimumEpochs[domain] != 1 || registry.CurrentEpochs[domain] != 1 {
			t.Fatalf("published registry omitted stable baseline domain %q: %#v", domain, registry)
		}
	}
	if !validFingerprint(digest) {
		t.Fatalf("published registry digest = %q", digest)
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
