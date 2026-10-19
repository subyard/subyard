package releasetransition

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pv1 "github.com/Subyard/Subyard/internal/releasetransition/protocol/v1"
)

// The existing Go constructors and codecs are the only format oracle. Export is
// opt-in; no Rust toolchain is required by normal Go tests.
func TestRustOwnerFoundationCorpus(t *testing.T) {
	destination := os.Getenv("SUBYARD_RUST_OWNER_CORPUS")
	if destination == "" {
		t.Skip("opt-in Rust owner corpus export")
	}
	catalog := registryV2TestCatalog(t)
	registry, registryDigest, err := ParseRegistryV2([]byte(validRegistryV2), catalog)
	if err != nil {
		t.Fatal(err)
	}
	ledger := BaselineLedgerV2(registry)
	projection, _, err := MarshalLedgerV2(ledger, registry)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := newLedgerCheckpointV1(ledger, registry, registryDigest, protectedSnapshotFromPayload(projection), registry.MinimumEpochs)
	if err != nil {
		t.Fatal(err)
	}
	type vector struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Input       string `json:"input"`
		Accepted    bool   `json:"accepted"`
		Canonical   string `json:"canonical"`
		RawDigest   string `json:"rawDigest"`
		SyntaxProbe bool   `json:"syntaxProbe"`
	}
	vectors := []vector{}
	type checkpointView struct {
		Name           string      `json:"name"`
		Registry       string      `json:"registry"`
		RegistryDigest Fingerprint `json:"registryDigest"`
		Checkpoint     string      `json:"checkpoint"`
		Accepted       bool        `json:"accepted"`
		Ledger         string      `json:"ledger"`
	}
	checkpointViews := []checkpointView{}
	bases := map[string][]byte{}
	encode := func(kind string, input []byte) ([]byte, error) {
		switch kind {
		case "registry":
			v, _, e := ParseRegistryV2(input, catalog)
			if e != nil {
				return nil, e
			}
			return json.Marshal(v)
		case "catalog":
			var d []CapabilityDescriptor
			if e := decodeBoundedRecord(input, MaxRegistryV2Bytes, &d); e != nil {
				return nil, e
			}
			v, e := NewCapabilityCatalog(d)
			if e != nil {
				return nil, e
			}
			return json.Marshal(v.descriptors)
		case "ledger":
			v, _, e := ParseLedgerV2(input, registry)
			if e != nil {
				return nil, e
			}
			p, _, e := MarshalLedgerV2(v, registry)
			return p, e
		case "checkpoint":
			v, _, e := ParseLedgerCheckpointV1(input, registry, registryDigest, protectedSnapshotFromPayload(projection))
			if e != nil {
				return nil, e
			}
			p, _, e := MarshalLedgerCheckpointV1(v, registry, registryDigest, protectedSnapshotFromPayload(projection))
			return p, e
		case "journal":
			v, e := ParseJournal(input)
			if e != nil {
				return nil, e
			}
			return MarshalJournal(v)
		case "evidence":
			v, e := ParseEvidence(input)
			if e != nil {
				return nil, e
			}
			return MarshalEvidence(v)
		case "archive":
			v, e := ParseSupersededJournal(input)
			if e != nil {
				return nil, e
			}
			return MarshalSupersededJournal(v)
		case "compatibility":
			v, e := ParseCompatibilityEvidence(input)
			if e != nil {
				return nil, e
			}
			return MarshalCompatibilityEvidence(v)
		case "request":
			v, e := pv1.DecodeRequest(bytes.NewReader(input))
			if e != nil {
				return nil, e
			}
			var b bytes.Buffer
			e = pv1.EncodeRequest(&b, v)
			return b.Bytes(), e
		case "response":
			v, e := pv1.DecodeResponse(bytes.NewReader(input))
			if e != nil {
				return nil, e
			}
			var b bytes.Buffer
			e = pv1.EncodeResponse(&b, v)
			return b.Bytes(), e
		default:
			t.Fatal(kind)
			return nil, nil
		}
	}
	add := func(name, kind string, input []byte, syntax bool) {
		canonical, e := encode(kind, input)
		vectors = append(vectors, vector{name, kind, string(input), e == nil, string(canonical), string(fingerprintPayload(input)), syntax})
	}
	base := func(kind string, value any) []byte {
		p, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = encode(kind, p); e != nil {
			t.Fatalf("%s fixture: %v", kind, e)
		}
		add(kind, kind, p, false)
		bases[kind] = bytes.Clone(p)
		return p
	}
	journal := validJournal(ReleasePair{From: "old", Target: "new"})
	journalBytes := base("journal", journal)
	evidence := EvidenceRecord{SchemaVersion: 2, Transaction: journal.Transaction, Releases: journal.Releases, Step: journal.Steps[0].ID, Expected: digestA, Desired: digestB, Observed: digestA, Checkpoint: EvidenceCaptured}
	base("evidence", evidence)
	evidence.Checkpoint = EvidenceVerified
	evidence.Observed = digestB
	journal.Steps[0].Checkpoint = StepVerified
	journal.Steps[0].Evidence = &evidence
	journal.Checkpoint = JournalComplete
	complete := base("journal", journal)
	canonicalComplete, _ := MarshalJournal(journal)
	base("archive", SupersededJournalRecord{SchemaVersion: 1, AuthorizationPlan: journal.AuthorizationPlan, Replacement: JournalReplacement{Transaction: journal.Transaction, Fingerprint: fingerprintPayload(canonicalComplete), Reason: JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1"}, Journal: journal})
	base("compatibility", CompatibilityEvidence{SchemaVersion: 1, Kind: V2LegacyV1Import, Identity: digestA, From: "old", Previous: "previous"})
	base("registry", registry)
	base("catalog", catalog.descriptors)
	base("catalog", []CapabilityDescriptor{})
	base("catalog", []CapabilityDescriptor(nil))
	base("ledger", ledger)
	base("checkpoint", checkpoint)
	terminal := ledger
	for {
		path, e := registry.PendingPath(terminal)
		if e != nil {
			t.Fatal(e)
		}
		if len(path) == 0 {
			break
		}
		terminal, e = terminal.Advance(registry, path[0])
		if e != nil {
			t.Fatal(e)
		}
	}
	base("ledger", terminal)
	terminalCheckpoint, e := newLedgerCheckpointV1(terminal, registry, registryDigest, protectedSnapshotFromPayload(projection), registry.MinimumEpochs)
	if e != nil {
		t.Fatal(e)
	}
	base("checkpoint", terminalCheckpoint)
	// Exercise the actual owning load path and its returned authority, rather
	// than comparing only checkpoint serialization. Registry byte identity may
	// change while completed definitions/prefixes remain exactly the same.
	checkpointBytes, _, e := MarshalLedgerCheckpointV1(terminalCheckpoint, registry, registryDigest, protectedSnapshotFromPayload(projection))
	if e != nil {
		t.Fatal(e)
	}
	viewCase := func(name string, other RegistryV2, identity Fingerprint, want bool) {
		if e := other.Validate(catalog); e != nil {
			t.Fatalf("%s registry: %v", name, e)
		}
		raw, e := json.Marshal(other)
		if e != nil {
			t.Fatal(e)
		}
		state, e := ParseMigrationLedger(protectedSnapshotFromPayload(projection), protectedSnapshotFromPayload(checkpointBytes), other, identity)
		if (e == nil) != want {
			t.Fatalf("%s owning acceptance=%v: %v", name, e == nil, e)
		}
		var ledgerBytes []byte
		if e == nil {
			ledgerBytes, _, e = MarshalLedgerV2(state.Ledger, other)
			if e != nil {
				t.Fatal(e)
			}
		}
		checkpointViews = append(checkpointViews, checkpointView{name, string(raw), identity, string(checkpointBytes), want, string(ledgerBytes)})
	}
	viewCase("same-registry-history", registry, registryDigest, true)
	viewCase("new-registry-byte-digest-same-history", registry, fingerprintPayload([]byte("new registry byte identity")), true)
	renamed := registry
	renamed.Migrations = append([]MigrationDefinitionV2(nil), registry.Migrations...)
	renamed.Migrations[len(renamed.Migrations)-1].ID = "settings-v3-other"
	viewCase("renamed-completed-history", renamed, registryDigest, false)
	changedPin := registry
	changedPin.RetiredPrefixes = map[string]RetiredMigrationPrefixV2{"settings": {ThroughEpoch: 1, Digest: digestC}}
	viewCase("changed-retired-prefix", changedPin, registryDigest, false)
	retired, _, e := ParseRegistryV2([]byte(pinnedCheckpointRegistryV2Payload), catalog)
	if e != nil {
		t.Fatal(e)
	}
	prefix, e := retired.MigrationPrefixDigest("settings", 2)
	if e != nil {
		t.Fatal(e)
	}
	retired.MinimumEpochs["settings"] = 2
	retired.Migrations = retired.Migrations[1:]
	retired.RetiredPrefixes = map[string]RetiredMigrationPrefixV2{"settings": {ThroughEpoch: 2, Digest: prefix}}
	retiredBytes, e := json.Marshal(retired)
	if e != nil {
		t.Fatal(e)
	}
	add("registry-retired-prefix", "registry", retiredBytes, false)
	request := pv1.Request{SchemaVersion: 1, Mode: pv1.ModeInspect, RuntimeRoot: "/tmp/runtime", ConfigHome: "/tmp/config", Target: "new", Direction: pv1.DirectionActivateTarget, ArtifactDigest: string(digestA)}
	requestBytes := base("request", request)
	request.SourceIngress = &pv1.SourceIngressRequest{SchemaVersion: 1, Kind: pv1.SourceIngressPreGo, SourceRoot: "/tmp/source<&>\u2028\u2029", DataHome: "/tmp/data", BinDir: "/tmp/bin", RC: "/tmp/rc", LoginRC: "/tmp/login"}
	request.Replacement = &pv1.JournalReplacement{Transaction: "previous-tx", Fingerprint: string(digestA), Reason: pv1.JournalReplacementPostActivationScopeV0111, SourceVersion: "0.11.1"}
	request.Mode = pv1.ModeConverge
	request.Execution = &pv1.Execution{Plan: "opaque", Authorization: "opaque"}
	base("request", request)
	outcome := pv1.Outcome{Status: pv1.StatusReady, ReachedGoal: true, Active: "new", Target: "new", Code: pv1.CodeReady, Message: "Public <>& \u2028\u2029 facts"}
	response := pv1.Response{SchemaVersion: 1, ActivationReconciliationOwned: true, Outcome: &outcome}
	responseBytes := base("response", response)
	response.Outcome = nil
	response.Inspection = &pv1.Inspection{Plan: string(journal.AuthorizationPlan), Assessment: pv1.ActionAssessment{Action: "release.transition.v2", Effect: "mutation", Impacts: []string{"local-metadata", "persistent-data", "yard-runtime"}, Recovery: "reversible"}, Outcome: &outcome}
	base("response", response)
	mutate := func(kind, name string, change func(map[string]any)) {
		var object map[string]any
		decoder := json.NewDecoder(bytes.NewReader(bases[kind]))
		decoder.UseNumber()
		if err := decoder.Decode(&object); err != nil {
			t.Fatal(err)
		}
		change(object)
		payload, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		add(name, kind, payload, false)
	}
	mutate("journal", "journal-intent-rebinding", func(v map[string]any) { v["observationScope"] = string(digestB) })
	mutate("journal", "journal-unverified-activation", func(v map[string]any) { v["steps"].([]any)[0].(map[string]any)["checkpoint"] = "intent" })
	mutate("journal", "journal-evidence-rebinding", func(v map[string]any) {
		v["steps"].([]any)[0].(map[string]any)["evidence"].(map[string]any)["transaction"] = "foreign"
	})
	mutate("journal", "journal-duplicate-step", func(v map[string]any) { s := v["steps"].([]any); v["steps"] = append(s, s[0]) })
	mutate("journal", "journal-too-many-steps", func(v map[string]any) {
		s := v["steps"].([]any)[0]
		steps := make([]any, 257)
		for i := range steps {
			steps[i] = s
		}
		v["steps"] = steps
	})
	mutate("evidence", "evidence-observed-conflict", func(v map[string]any) { v["observedFingerprint"] = string(digestC) })
	mutate("evidence", "evidence-invalid-recovery", func(v map[string]any) { v["recoveryFingerprint"] = "foreign" })
	mutate("archive", "archive-byte-rebinding", func(v map[string]any) { v["replacement"].(map[string]any)["fingerprint"] = string(digestA) })
	mutate("archive", "archive-source-version", func(v map[string]any) { v["replacement"].(map[string]any)["sourceVersion"] = "01.11.1" })
	mutate("compatibility", "compatibility-identical-releases", func(v map[string]any) { v["previous"] = v["from"] })
	mutate("request", "request-unclean-ingress", func(v map[string]any) { v["sourceIngress"].(map[string]any)["sourceRoot"] = "/tmp/../source" })
	mutate("request", "request-missing-replacement-version", func(v map[string]any) { v["replacement"].(map[string]any)["sourceVersion"] = "" })
	mutate("request", "request-inspect-with-execution", func(v map[string]any) { v["mode"] = "inspect" })
	mutate("response", "response-assessment-missing-consequence", func(v map[string]any) {
		v["inspection"].(map[string]any)["assessment"].(map[string]any)["changed"] = true
	})
	mutate("response", "response-unknown-outcome-code", func(v map[string]any) {
		v["inspection"].(map[string]any)["outcome"].(map[string]any)["code"] = "foreign"
	})
	mutate("response", "response-warning-order", func(v map[string]any) {
		v["inspection"].(map[string]any)["outcome"].(map[string]any)["warnings"] = []string{"z", "a"}
	})
	mutate("registry", "registry-unsupported-kind", func(v map[string]any) { v["migrations"].([]any)[0].(map[string]any)["kind"] = "foreign" })
	mutate("registry", "registry-foreign-dependency", func(v map[string]any) { v["migrations"].([]any)[0].(map[string]any)["dependsOn"] = []string{"foreign"} })
	mutate("registry", "registry-retired-wrong-floor", func(v map[string]any) {
		v["retiredPrefixes"] = map[string]any{"settings": map[string]any{"throughEpoch": 2, "digest": digestA}}
	})
	add("ledger-null-applied-baseline", "ledger", []byte(strings.ReplaceAll(string(projection), `"applied":[]`, `"applied":null`)), false)
	mutate("ledger", "ledger-null-applied-completed", func(v map[string]any) { v["domains"].(map[string]any)["settings"].(map[string]any)["applied"] = nil })
	mutate("ledger", "ledger-foreign-applied", func(v map[string]any) {
		v["domains"].(map[string]any)["settings"].(map[string]any)["applied"] = []string{"foreign"}
	})
	mutate("checkpoint", "checkpoint-prefix-rebinding", func(v map[string]any) {
		v["domains"].(map[string]any)["settings"].(map[string]any)["prefixDigest"] = string(digestC)
	})
	mutate("checkpoint", "checkpoint-projection-rebinding", func(v map[string]any) { v["legacyProjection"].(map[string]any)["fingerprint"] = string(digestC) })
	mutate("checkpoint", "checkpoint-registry-suffix-identity", func(v map[string]any) { v["registryDigest"] = string(digestC) })
	for name, descriptors := range map[string][]CapabilityDescriptor{
		"catalog-duplicate":       {catalog.descriptors[0], catalog.descriptors[0]},
		"catalog-invalid-version": {{Kind: "kind", Domain: "settings", Version: 0}},
		"catalog-unknown-policy":  {{Kind: "kind", Domain: "settings", Version: 1, RollbackCompatibility: "foreign"}},
	} {
		raw, _ := json.Marshal(descriptors)
		add(name, "catalog", raw, false)
	}
	for _, item := range []struct {
		kind string
		data []byte
	}{{"journal", journalBytes}, {"journal", complete}, {"request", requestBytes}, {"response", responseBytes}, {"registry", []byte(validRegistryV2)}, {"ledger", projection}} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(item.data, &object); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"schemaVersion", "unknown"} {
			before, had := object[key]
			object[key] = json.RawMessage(`999`)
			mutated, _ := json.Marshal(object)
			add(item.kind+"-bad-"+key, item.kind, mutated, false)
			if had {
				object[key] = before
			} else {
				delete(object, key)
			}
		}
		add(item.kind+"-trailing", item.kind, append(bytes.Clone(item.data), []byte("{}")...), false)
	}
	// Actual decoder probes document accepted syntax; canonical Rust admission
	// is separate and must not be misreported as the frozen accepted set.
	for _, kind := range []string{"registry", "journal", "request", "response"} {
		var raw []byte
		switch kind {
		case "registry":
			raw = []byte(validRegistryV2)
		case "journal":
			raw = journalBytes
		case "request":
			raw = requestBytes
		case "response":
			raw = responseBytes
		}
		for label, mutate := range map[string]func(string) string{
			"duplicate": func(s string) string {
				return strings.Replace(s, `"schemaVersion":`, `"schemaVersion":999,"schemaVersion":`, 1)
			},
			"folded": func(s string) string { return strings.Replace(s, `"schemaVersion"`, `"SCHEMAVERSION"`, 1) },
			"null-before": func(s string) string {
				return strings.Replace(s, `"schemaVersion":`, `"schemaVersion":null,"schemaVersion":`, 1)
			},
		} {
			add(kind+"-"+label, kind, []byte(mutate(string(raw))), true)
		}
	}
	corpus := struct {
		Catalog         []CapabilityDescriptor `json:"catalog"`
		CatalogDigest   Fingerprint            `json:"catalogDigest"`
		Registry        string                 `json:"registry"`
		RegistryDigest  Fingerprint            `json:"registryDigest"`
		Projection      string                 `json:"projection"`
		Vectors         []vector               `json:"vectors"`
		CheckpointViews []checkpointView       `json:"checkpointViews"`
	}{catalog.descriptors, catalog.Digest(), validRegistryV2, registryDigest, string(projection), vectors, checkpointViews}
	payload, err := json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("exported %d Go-owned format vectors (%d bytes)", len(vectors), len(payload)+1)
	exportRustOwnerRecoveryCorpus(t, destination+".recovery.json")
}
