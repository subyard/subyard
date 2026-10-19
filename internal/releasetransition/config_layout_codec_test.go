package releasetransition

import "testing"

func TestConfigLayoutCodecPreservesUnmarshalGrammarWithoutEligibility(t *testing.T) {
	for _, sample := range []struct{ raw, expected string }{
		{`{"schemaVersion":2,"files":[{"path":"yards/x/config.env","extra":true}],"extra":true}`, `{"schemaVersion":2,"files":[{"path":"yards/x/config.env"}]}`},
		{`{"SCHEMAVERSION":1,"schemaVersion":2,"FILES":[{"PATH":"unsafe/path"},{"path":null},null]}`, `{"schemaVersion":2,"files":[{"path":"unsafe/path"},{"path":""},{"path":""}]}`},
		{`{"schemaVersion":null,"files":null}`, `{"schemaVersion":0,"files":null}`},
		{`{}`, `{"schemaVersion":0,"files":null}`},
	} {
		projection, flags, err := normalizeLegacy(17, []byte(sample.raw))
		if err != nil || flags != 0 || string(projection) != sample.expected {
			t.Fatalf("layout syntax changed: %s => %s/%d: %v", sample.raw, projection, flags, err)
		}
	}
	for _, raw := range []string{`{"files":[{"path":3}]}`, `{"files":true}`, `{"schemaVersion":1.5}`, `{} {}`} {
		if _, _, err := normalizeLegacy(17, []byte(raw)); err == nil {
			t.Fatalf("unsupported layout syntax admitted: %s", raw)
		}
	}
}
