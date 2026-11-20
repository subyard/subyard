package releasetransition

import (
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
			if (err == nil) != test.forward || predecessor.Digest != map[bool]Fingerprint{true: digest}[test.forward] {
				t.Fatalf("forward parse = %q, %v", predecessor, err)
			}
		})
	}
}
