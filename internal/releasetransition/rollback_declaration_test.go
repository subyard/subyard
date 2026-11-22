package releasetransition

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A release parses its own declaration strictly, but reads another release's
// only for the predecessor digest, so a newer schema stays readable.
func TestRollbackPredecessorToleratesNewerDeclarations(t *testing.T) {
	digest := Fingerprint(strings.Repeat("a", 64))
	declaration := func(format string) []byte { return []byte(fmt.Sprintf(format, digest)) }
	for _, test := range []struct {
		name            string
		payload         []byte
		strict, forward bool
	}{
		{name: "schema 1", payload: declaration(`{"schemaVersion":1,"predecessorRegistryDigest":%q,"migrations":[],"profileFormats":[]}`), strict: true, forward: true},
		{name: "unknown field", payload: declaration(`{"schemaVersion":1,"predecessorRegistryDigest":%q,"migrations":[],"profileFormats":[],"registryFeatures":["retired-dependencies"]}`), forward: true},
		{name: "higher schema", payload: declaration(`{"schemaVersion":2,"predecessorRegistryDigest":%q}`), forward: true},
		{name: "entries", payload: declaration(`{"schemaVersion":1,"predecessorRegistryDigest":%q,"migrations":[{"id":"future"}],"profileFormats":[{"profile":"future"}]}`), forward: true},
		{name: "malformed digest", payload: []byte(`{"schemaVersion":1,"predecessorRegistryDigest":"AAAA","migrations":[],"profileFormats":[]}`)},
		{name: "missing digest", payload: []byte(`{"schemaVersion":2}`)},
		{name: "missing schema", payload: declaration(`{"predecessorRegistryDigest":%q}`)},
		{name: "schema 0", payload: declaration(`{"schemaVersion":0,"predecessorRegistryDigest":%q}`)},
		{name: "not an object", payload: []byte(`[1]`)},
		{name: "null", payload: []byte(`null`)},
		{name: "invalid JSON", payload: []byte(`{"schemaVersion":1,`)},
		{name: "trailing JSON", payload: declaration(`{"schemaVersion":1,"predecessorRegistryDigest":%q} {}`)},
		{name: "empty", payload: []byte{}},
		{name: "oversize", payload: append(declaration(`{"schemaVersion":1,"predecessorRegistryDigest":%q}`), []byte(strings.Repeat(" ", MaxRollbackDeclarationBytes))...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			own, err := ParseRollbackDeclaration(test.payload)
			if (err == nil) != test.strict || test.strict && own.PredecessorRegistryDigest != digest {
				t.Fatalf("strict parse = %#v, %v", own, err)
			}
			predecessor, err := ParseRollbackPredecessor(test.payload)
			if (err == nil) != test.forward || predecessor != map[bool]Fingerprint{true: digest}[test.forward] {
				t.Fatalf("forward parse = %q, %v", predecessor, err)
			}
		})
	}
}

func TestRollbackRegistryFeaturesAreSealedAndForwardTolerant(t *testing.T) {
	for _, test := range []struct {
		features        string
		strict, capable bool
	}{
		{features: `[]`, strict: true},
		{features: `["retired-prefix-dependencies"]`, strict: true, capable: true},
		{features: `["future"]`},
		{features: `["future", "retired-prefix-dependencies", {"future":true}, 7]`, capable: true},
		{features: `["retired-prefix-dependencies", "retired-prefix-dependencies"]`, capable: true},
	} {
		payload := []byte(fmt.Sprintf(`{"schemaVersion":1,"predecessorRegistryDigest":%q,"migrations":[],"profileFormats":[],"registryFeatures":%s}`, strings.Repeat("a", 64), test.features))
		_, err := ParseRollbackDeclaration(payload)
		if (err == nil) != test.strict {
			t.Fatalf("features %s: strict error %v", test.features, err)
		}
		features, err := ParseRollbackRegistryFeatures(payload)
		if err != nil || (len(features) == 1 && features[0] == RegistryFeatureRetiredPrefixDependencies) != test.capable || len(features) > 1 {
			t.Fatalf("features %s: external %v, %v", test.features, features, err)
		}
	}
}

func TestRollbackBoundaryValidation(t *testing.T) {
	base := RollbackDeclaration{SchemaVersion: 1, PredecessorRegistryDigest: Fingerprint(strings.Repeat("a", 64)),
		Migrations: []json.RawMessage{}, ProfileFormats: []json.RawMessage{}}
	for _, test := range []struct {
		name     string
		boundary RollbackBoundary
		valid    bool
	}{
		{name: "registry", boundary: RollbackBoundary{Component: "registry", ThroughVersion: "0.30.0", Reason: "unreadable registry", Recovery: "keep current release active"}, valid: true},
		{name: "profile", boundary: RollbackBoundary{Component: "profile:sample-runtime", ThroughVersion: "0.30.0", Reason: "unreadable state", Recovery: "restore saved state"}, valid: true},
		{name: "agents", boundary: RollbackBoundary{Component: "agents", ThroughVersion: "0.30.0", Reason: "surviving policy", Recovery: "remove the listed managed files", SourceDigest: Fingerprint(strings.Repeat("b", 64)), Files: []string{"/etc/sample/policy.json", "~/.sample/config"}}, valid: true},
		{name: "unknown component", boundary: RollbackBoundary{Component: "future", ThroughVersion: "0.30.0", Reason: "state", Recovery: "restore"}},
		{name: "invalid version", boundary: RollbackBoundary{Component: "registry", ThroughVersion: "v0.30", Reason: "state", Recovery: "restore"}},
		{name: "missing recovery", boundary: RollbackBoundary{Component: "registry", ThroughVersion: "0.30.0", Reason: "state"}},
		{name: "control character", boundary: RollbackBoundary{Component: "registry", ThroughVersion: "0.30.0", Reason: "state\x1b", Recovery: "restore"}},
		{name: "missing agent files", boundary: RollbackBoundary{Component: "agents", ThroughVersion: "0.30.0", Reason: "state", Recovery: "restore", SourceDigest: Fingerprint(strings.Repeat("b", 64))}},
		{name: "unsafe agent files", boundary: RollbackBoundary{Component: "agents", ThroughVersion: "0.30.0", Reason: "state", Recovery: "restore", SourceDigest: Fingerprint(strings.Repeat("b", 64)), Files: []string{"/etc/../policy"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			base.Boundaries = []RollbackBoundary{test.boundary}
			payload, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ParseRollbackDeclaration(payload)
			if (err == nil) != test.valid {
				t.Fatalf("validation = %v", err)
			}
			if _, err := ParseRollbackPredecessor(payload); err != nil {
				t.Fatalf("external predecessor reader rejected future fields: %v", err)
			}
		})
	}
}

func TestRollbackBoundaryOrderingUsesNumericVersions(t *testing.T) {
	declaration := RollbackDeclaration{SchemaVersion: 1, PredecessorRegistryDigest: Fingerprint(strings.Repeat("a", 64)), Migrations: []json.RawMessage{}, ProfileFormats: []json.RawMessage{}}
	for _, version := range []string{"0.9.0", "0.10.0"} {
		declaration.Boundaries = append(declaration.Boundaries, RollbackBoundary{Component: "registry", ThroughVersion: version, Reason: "unreadable state", Recovery: "restore backup"})
	}
	data, _ := json.Marshal(declaration)
	if _, err := ParseRollbackDeclaration(data); err != nil {
		t.Fatal(err)
	}
	declaration.Boundaries[0], declaration.Boundaries[1] = declaration.Boundaries[1], declaration.Boundaries[0]
	data, _ = json.Marshal(declaration)
	if _, err := ParseRollbackDeclaration(data); err == nil {
		t.Fatal("lexicographic version ordering accepted")
	}
}
