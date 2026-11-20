package releasetransition

import (
	"context"
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

func TestPlanDownPathAdmitsOnlyRetiredPrefixRepresentation(t *testing.T) {
	v030, candidate := publishedV030Registry(t), decodeRetiringCandidate(t)
	mutated := func(change func(*RegistryV2)) RegistryV2 {
		registry := decodeRetiringCandidate(t)
		change(&registry)
		return registry
	}
	retiredFurther, err := v030.RetirePrefixes(map[string]int{"settings": 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name           string
		active, target RegistryV2
		kind           HistoryCompatibilityKind
	}{
		{name: "one registry", active: v030, target: v030, kind: HistoryIdentity},
		{name: "retired prefixes of the target", active: candidate, target: v030, kind: HistoryEquivalent},
		{name: "older target", active: v030, target: olderHistoryRegistry(v030), kind: HistoryRefuse},
		{name: "retired digest differs", kind: HistoryRefuse, target: v030, active: mutated(func(registry *RegistryV2) {
			prefix := registry.RetiredPrefixes["settings"]
			prefix.Digest = Fingerprint(strings.Repeat("d", 64))
			registry.RetiredPrefixes["settings"] = prefix
		})},
		// Later definitions are chained into the prefix digests of later history.
		{name: "later definition differs", kind: HistoryRefuse, target: v030, active: mutated(func(registry *RegistryV2) {
			registry.Migrations[0].DependsOn = nil
		})},
		{name: "later definition missing", kind: HistoryRefuse, target: v030, active: mutated(func(registry *RegistryV2) {
			registry.Migrations, registry.CurrentEpochs["power-metadata"] = nil, 3
		})},
		{name: "floor beyond the target", kind: HistoryRefuse, target: olderHistoryRegistry(v030), active: candidate},
		{name: "target retires more", kind: HistoryRefuse, active: candidate, target: mutated(func(registry *RegistryV2) {
			registry.RetiredPrefixes["project-state"] = RetiredMigrationPrefixV2{ThroughEpoch: 1, Digest: Fingerprint(strings.Repeat("e", 64))}
		})},
		{name: "unretired active", kind: HistoryRefuse, active: v030, target: retiredFurther},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := PlanDownPath(test.active, test.target)
			if err != nil || path.Kind != test.kind {
				t.Fatalf("down path = %#v, %v", path, err)
			}
			if want, _ := test.target.CanonicalDigest(); path.Target != want {
				t.Fatalf("down path target %s, want %s", path.Target, want)
			}
		})
	}
}

// publishedV030History is the protected history the v0.30.0 tag engine wrote for
// a completed installation with a checkpoint
// (testdata/v0.30.0-completed-install-checkpointed.json, sha256 8458d868...).
func publishedV030History(t *testing.T) (ProtectedSnapshot, ProtectedSnapshot) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", "v0.30.0-completed-install-checkpointed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var files map[string]string
	if err := json.Unmarshal(payload, &files); err != nil {
		t.Fatal(err)
	}
	return protectedSnapshotFromPayload([]byte(files["release-transition/v2/ledger.json"])),
		protectedSnapshotFromPayload([]byte(files["release-transition/v2/history-checkpoint.json"]))
}

// The v0.30.0 target reads history its retiring successor wrote: checkpoint
// reading checks prefix digests and epochs, not the writer's registry digest.
func TestHistoryCompatibilityAdmitsDeclaredEquivalentPredecessor(t *testing.T) {
	v030, candidate := publishedV030Registry(t), decodeRetiringCandidate(t)
	v030Digest := fingerprintPayload(mustReadTestdata(t, "v0.30.0-release-transition.json"))
	predecessor, err := v030.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	ledger, checkpoint := publishedV030History(t)
	var written LedgerCheckpointV1
	if err := json.Unmarshal(checkpoint.Payload, &written); err != nil {
		t.Fatal(err)
	}
	written.RegistryDigest = fingerprintPayload([]byte(retiringCandidateRegistry))
	rewritten, err := json.Marshal(written)
	if err != nil {
		t.Fatal(err)
	}
	successor := protectedSnapshotFromPayload(append(rewritten, '\n'))
	evidence := func(checkpoint ProtectedSnapshot) *HistoryEvidence {
		return &HistoryEvidence{Registry: v030, RegistryDigest: v030Digest, Ledger: ledger, Checkpoint: checkpoint, CheckpointReader: true}
	}
	missing := absentProtectedSnapshot()
	corrupt := protectedSnapshotFromPayload([]byte("{}"))
	differs := "the selected release's migration registry differs from the active release's registry"
	for _, test := range []struct {
		name        string
		input       HistoryCompatibilityInput
		kind        HistoryCompatibilityKind
		predecessor bool
		reason      HistoryRefusalReason
		message     string
	}{
		{name: "v0.30.0 checkpoint", kind: HistoryEquivalent, predecessor: true,
			input: HistoryCompatibilityInput{Active: &candidate, Target: &v030, Predecessor: predecessor, History: evidence(checkpoint)}},
		{name: "successor checkpoint", kind: HistoryEquivalent, predecessor: true,
			input: HistoryCompatibilityInput{Active: &candidate, Target: &v030, Predecessor: predecessor, History: evidence(successor)}},
		{name: "legacy ledger", kind: HistoryEquivalent, predecessor: true,
			input: HistoryCompatibilityInput{Active: &candidate, Target: &v030, Predecessor: predecessor, History: evidence(missing)}},
		{name: "undeclared skips history", kind: HistoryEquivalent, reason: HistoryRegistryDiffers, message: differs,
			input: HistoryCompatibilityInput{Active: &candidate, Target: &v030, Predecessor: Fingerprint(strings.Repeat("f", 64)), History: evidence(corrupt)}},
		{name: "no declaration", kind: HistoryEquivalent, reason: HistoryRegistryDiffers, message: differs,
			input: HistoryCompatibilityInput{Active: &candidate, Target: &v030, History: evidence(checkpoint)}},
		{name: "declared but unreadable history", kind: HistoryEquivalent, predecessor: true, reason: HistoryUnverifiable,
			message: "the selected release cannot verify the authoritative active migration history",
			input:   HistoryCompatibilityInput{Active: &candidate, Target: &v030, Predecessor: predecessor, History: evidence(corrupt)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			verdict := HistoryCompatibility(test.input)
			if verdict.Kind != test.kind || verdict.Registry != RegistryDiffers || verdict.DeclaredPredecessor != test.predecessor {
				t.Fatalf("verdict = %#v", verdict)
			}
			message, _ := verdict.Refusal()
			if test.reason == "" {
				if !verdict.Admitted() || message != "" {
					t.Fatalf("equivalent predecessor refused: %#v", verdict)
				}
				return
			}
			if verdict.Admitted() || len(verdict.Refusals) != 1 || verdict.Refusals[0].Reason != test.reason || message != test.message {
				t.Fatalf("refusal = %#v", verdict)
			}
		})
	}
	// The declared predecessor alone admits nothing when the registries are not equivalent.
	older := olderHistoryRegistry(v030)
	olderDigest, _ := older.CanonicalDigest()
	verdict := HistoryCompatibility(HistoryCompatibilityInput{Active: &candidate, Target: &older, Predecessor: olderDigest})
	if verdict.Admitted() || verdict.Kind != HistoryRefuse || !verdict.DeclaredPredecessor {
		t.Fatalf("declared non-equivalent predecessor = %#v", verdict)
	}
}

func mustReadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// An engine whose registry retires prefixes admits its declared predecessor.
func TestV2RollbackRegistryAdmitsDeclaredEquivalentPredecessor(t *testing.T) {
	full := v2TestRegistry(t)
	retired, err := full.RetirePrefixes(map[string]int{"project-state": 1})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(retired)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err := full.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	other := Fingerprint(strings.Repeat("c", 64))
	for name, test := range map[string]struct {
		predecessor Fingerprint
		refused     bool
	}{
		"declared":   {predecessor: predecessor},
		"undeclared": {predecessor: other, refused: true},
		"none":       {refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newV2RollbackBoundaryFixture(t, "0.30.0", other, payload)
			fixture.transition.options.RollbackTargetRegistry = &full
			fixture.transition.options.RollbackPredecessor = test.predecessor
			if refused := fixture.transition.rollbackRegistryRefused(); refused != test.refused {
				t.Fatalf("rollback registry refused = %v", refused)
			}
			observation, err := fixture.transition.observe(context.Background(), Goal{Target: "release-a", Direction: DirectionActivatePrevious})
			blocked := err == nil && len(observation.blockers) == 1 && observation.blockers[0].Code == CodeRollbackIncompatible
			if err != nil || blocked != test.refused {
				t.Fatalf("rollback observation blockers = %#v, err=%v", observation.blockers, err)
			}
			fixture.assertUnchanged(t, false)
		})
	}
}
