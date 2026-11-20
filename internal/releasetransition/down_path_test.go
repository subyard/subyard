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

// retiringCandidate is the registry of a successor that retires every
// completed v0.30.0 prefix except the newest power-metadata step
// (testdata/retiring-candidate-registry.json, sha256 474b7158...). Its
// surviving NM guard definition depends on a retired migration, which the
// v0.30.0 parser and this engine's do not accept, so it is decoded without
// validation.
func retiringCandidate(t *testing.T) RegistryV2 {
	t.Helper()
	var registry RegistryV2
	if err := json.Unmarshal(mustReadTestdata(t, "retiring-candidate-registry.json"), &registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

func retire(t *testing.T, registry RegistryV2, floors map[string]int) RegistryV2 {
	t.Helper()
	retired, err := registry.RetirePrefixes(floors)
	if err != nil {
		t.Fatal(err)
	}
	return retired
}

// The candidate's retired digests are the v0.30.0 prefix digests, yet the
// pair is refused: v0.30.0 cannot parse the candidate registry.
func TestRetirePrefixesReproducesPublishedPrefixDigests(t *testing.T) {
	v030, candidate := publishedV030Registry(t), retiringCandidate(t)
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
	path, err := PlanDownPath(candidate, v030)
	if err != nil || path.Kind != HistoryRefuse || path.Unreadable == nil ||
		!strings.Contains(path.Unreadable.Error(), `"complete-registered-guest-disk-tmp-v3"`) {
		t.Fatalf("candidate down path = %#v, %v", path, err)
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
	v030 := publishedV030Registry(t)
	projectState := retire(t, v030, map[string]int{"project-state": 1})
	mutated := func(change func(*RegistryV2)) RegistryV2 {
		registry := retire(t, v030, map[string]int{"project-state": 1})
		change(&registry)
		return registry
	}
	// Retiring power-metadata through 4 leaves no definition that depends on
	// retired history.
	fullyRetired := retire(t, v030, map[string]int{"owner-registration": 3, "power-metadata": 4, "settings": 3})
	powerOnly := retire(t, v030, map[string]int{"power-metadata": 4})
	for _, test := range []struct {
		name                  string
		active, target        RegistryV2
		kind                  HistoryCompatibilityKind
		successor, unreadable bool
	}{
		{name: "one registry", active: v030, target: v030, kind: HistoryIdentity},
		{name: "retired prefixes of the target", active: projectState, target: v030, kind: HistoryEquivalent},
		{name: "every prefix retired", active: fullyRetired, target: v030, kind: HistoryEquivalent},
		{name: "successor retires prefixes", active: v030, target: projectState, kind: HistoryEquivalent, successor: true},
		// The successor's own engine parses its registry; this release parses
		// it only when it selects that target.
		{name: "successor with retired dependencies", active: v030, target: retiringCandidate(t), kind: HistoryEquivalent, successor: true},
		{name: "surviving dependency on retired history", active: retiringCandidate(t), target: v030, kind: HistoryRefuse, unreadable: true},
		{name: "older target", active: v030, target: olderHistoryRegistry(v030), kind: HistoryRefuse},
		{name: "retired digest differs", kind: HistoryRefuse, target: v030, active: mutated(func(registry *RegistryV2) {
			prefix := registry.RetiredPrefixes["project-state"]
			prefix.Digest = Fingerprint(strings.Repeat("d", 64))
			registry.RetiredPrefixes["project-state"] = prefix
		})},
		{name: "later definition differs", kind: HistoryRefuse, target: v030, active: mutated(func(registry *RegistryV2) {
			registry.Migrations[len(registry.Migrations)-1].DependsOn = nil
		})},
		{name: "later definition missing", kind: HistoryRefuse, target: v030, active: olderHistoryRegistry(projectState)},
		{name: "floor beyond the target", kind: HistoryRefuse, active: powerOnly, target: olderHistoryRegistry(v030)},
		{name: "different retired domains", kind: HistoryRefuse, active: projectState, target: powerOnly},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := PlanDownPath(test.active, test.target)
			if err != nil || path.Kind != test.kind || path.Successor != test.successor || (path.Unreadable != nil) != test.unreadable {
				t.Fatalf("down path = %#v, %v", path, err)
			}
			active, _ := test.active.CanonicalDigest()
			if target, _ := test.target.CanonicalDigest(); path.Target != target || path.Active != active {
				t.Fatalf("down path digests %s/%s, want %s/%s", path.Active, path.Target, active, target)
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
// The successor reads v0.30.0 history the same way after it returns.
func TestHistoryCompatibilityAdmitsEquivalentWithinTheDeclaredPair(t *testing.T) {
	v030 := publishedV030Registry(t)
	retired := retire(t, v030, map[string]int{"project-state": 1})
	retiredPayload, err := json.Marshal(retired)
	if err != nil {
		t.Fatal(err)
	}
	candidate := retiringCandidate(t)
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
	written.RegistryDigest = fingerprintPayload(retiredPayload)
	rewritten, err := json.Marshal(written)
	if err != nil {
		t.Fatal(err)
	}
	successor := protectedSnapshotFromPayload(append(rewritten, '\n'))
	evidence := func(registry RegistryV2, digest Fingerprint, checkpoint ProtectedSnapshot) *HistoryEvidence {
		return &HistoryEvidence{Registry: registry, RegistryDigest: digest, Ledger: ledger, Checkpoint: checkpoint, CheckpointReader: true}
	}
	v030History := func(checkpoint ProtectedSnapshot) *HistoryEvidence { return evidence(v030, v030Digest, checkpoint) }
	missing := absentProtectedSnapshot()
	corrupt := protectedSnapshotFromPayload([]byte("{}"))
	other := Fingerprint(strings.Repeat("f", 64))
	differs := "the selected release's migration registry differs from the active release's registry"
	for _, test := range []struct {
		name                   string
		input                  HistoryCompatibilityInput
		kind                   HistoryCompatibilityKind
		predecessor, successor bool
		reason                 HistoryRefusalReason
		message                string
	}{
		{name: "v0.30.0 checkpoint", kind: HistoryEquivalent, predecessor: true,
			input: HistoryCompatibilityInput{Active: &retired, Target: &v030, Predecessor: predecessor, History: v030History(checkpoint)}},
		{name: "successor checkpoint", kind: HistoryEquivalent, predecessor: true,
			input: HistoryCompatibilityInput{Active: &retired, Target: &v030, Predecessor: predecessor, History: v030History(successor)}},
		{name: "legacy ledger", kind: HistoryEquivalent, predecessor: true,
			input: HistoryCompatibilityInput{Active: &retired, Target: &v030, Predecessor: predecessor, History: v030History(missing)}},
		{name: "undeclared skips history", kind: HistoryEquivalent, reason: HistoryRegistryDiffers, message: differs,
			input: HistoryCompatibilityInput{Active: &retired, Target: &v030, Predecessor: other, History: v030History(corrupt)}},
		{name: "no declaration", kind: HistoryEquivalent, reason: HistoryRegistryDiffers, message: differs,
			input: HistoryCompatibilityInput{Active: &retired, Target: &v030, History: v030History(checkpoint)}},
		{name: "declared but unreadable history", kind: HistoryEquivalent, predecessor: true, reason: HistoryUnverifiable,
			message: "the selected release cannot verify the authoritative active migration history",
			input:   HistoryCompatibilityInput{Active: &retired, Target: &v030, Predecessor: predecessor, History: v030History(corrupt)}},
		{name: "declared but unparseable active registry", kind: HistoryRefuse, predecessor: true, reason: HistoryRegistryUnreadable,
			message: "the selected release cannot parse the active release's migration registry",
			input:   HistoryCompatibilityInput{Active: &candidate, Target: &v030, Predecessor: predecessor, History: v030History(corrupt)}},
		{name: "return to the declaring successor", kind: HistoryEquivalent, successor: true,
			input: HistoryCompatibilityInput{Active: &v030, Target: &retired, TargetPredecessor: predecessor,
				History: evidence(retired, fingerprintPayload(retiredPayload), successor)}},
		{name: "successor that declares another predecessor", kind: HistoryEquivalent, reason: HistoryRegistryDiffers, message: differs,
			input: HistoryCompatibilityInput{Active: &v030, Target: &retired, TargetPredecessor: other, Predecessor: predecessor}},
	} {
		t.Run(test.name, func(t *testing.T) {
			verdict := HistoryCompatibility(test.input)
			if verdict.Kind != test.kind || verdict.Registry != RegistryDiffers || verdict.DeclaredPredecessor != test.predecessor ||
				verdict.DeclaredSuccessor != test.successor {
				t.Fatalf("verdict = %#v", verdict)
			}
			message, _ := verdict.Refusal()
			if test.reason == "" {
				if !verdict.Admitted() || message != "" {
					t.Fatalf("equivalent pair refused: %#v", verdict)
				}
				return
			}
			if verdict.Admitted() || len(verdict.Refusals) != 1 || verdict.Refusals[0].Reason != test.reason || message != test.message {
				t.Fatalf("refusal = %#v", verdict)
			}
		})
	}
	// A declaration alone admits nothing when the registries are not equivalent.
	older := olderHistoryRegistry(v030)
	olderDigest, _ := older.CanonicalDigest()
	verdict := HistoryCompatibility(HistoryCompatibilityInput{Active: &retired, Target: &older, Predecessor: olderDigest})
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
