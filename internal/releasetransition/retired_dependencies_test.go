package releasetransition

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// This golden fixes the entire additional language, including strict-first
// cases with non-closed retired floors. A future registry is not a new family.
func TestFrozenV030RetirementLanguage(t *testing.T) {
	base := publishedV030Registry(t)
	golden, err := os.ReadFile("testdata/v0.30.0-readable-retirements.txt")
	if err != nil {
		t.Fatal(err)
	}
	var accepted strings.Builder
	accepted.WriteString("# owner-registration power-metadata settings project-state-marker rule\n")
	count, strictCount, total := 0, 0, 0
	for owner := 1; owner <= 3; owner++ {
		for power := 1; power <= 4; power++ {
			for settings := 1; settings <= 3; settings++ {
				for project := 0; project <= 1; project++ {
					floors := map[string]int{}
					for domain, floor := range map[string]int{"owner-registration": owner, "power-metadata": power, "settings": settings, "project-state": project} {
						if floor > 1 || domain == "project-state" && floor == 1 {
							floors[domain] = floor
						}
					}
					if len(floors) == 0 {
						continue
					}
					total++
					retired := retire(t, base, floors)
					payload, err := json.MarshalIndent(retired, "", "  ")
					if err != nil {
						t.Fatal(err)
					}
					parsed, digest, readErr := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
					_, _, strictErr := ParseRegistryV2WithFeatures(payload, BuiltinCapabilityCatalog(), nil)
					if (strictErr == nil) != (v030RegistryDependencies(retired.Migrations) == nil) {
						t.Fatalf("floors %v: strict parser changed: %v", floors, strictErr)
					}
					if readErr != nil {
						if digest != "" || !reflect.DeepEqual(parsed, RegistryV2{}) {
							t.Fatal("failed parser returned partial output")
						}
						continue
					}
					count++
					if digest != fingerprintPayload(payload) || !reflect.DeepEqual(parsed, retired) {
						t.Fatalf("floors %v: parser changed registry or raw digest", floors)
					}
					rule := "closed"
					if strictErr == nil {
						rule = "strict"
						strictCount++
					}
					fmt.Fprintf(&accepted, "%d %d %d %d %s\n", owner, power, settings, project, rule)
					// Every newly accepted registry must support all live steps
					// from its floor; retired IDs never appear as live work.
					history, err := parsed.historyRegistry()
					if err != nil {
						t.Fatal(err)
					}
					state, err := history.Baseline()
					if err != nil {
						t.Fatal(err)
					}
					for range parsed.Migrations {
						edge, err := history.Next(state)
						if err != nil {
							t.Fatalf("floors %v: %v", floors, err)
						}
						state = edge.After()
					}
					if _, err := history.Complete(state); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	if accepted.String() != string(golden) || total != 71 || count != 25 || strictCount != 5 {
		t.Fatalf("retirements total/accepted/strict = %d/%d/%d; golden differs:\n%s", total, count, strictCount, accepted.String())
	}
}

func TestRetiredDependencyReaderIsClosed(t *testing.T) {
	for name, change := range map[string]func(*RegistryV2){
		"foreign live id":      func(r *RegistryV2) { r.Migrations[0].ID = "foreign" },
		"reused retired id":    func(r *RegistryV2) { r.Migrations[0].ID = "canonicalize-test-vms-settings-v2" },
		"foreign kind":         func(r *RegistryV2) { r.Migrations[0].Kind = "owner-publication-state-v1" },
		"foreign dependency":   func(r *RegistryV2) { r.Migrations[0].DependsOn = []string{"foreign"} },
		"duplicate definition": func(r *RegistryV2) { r.Migrations = append(r.Migrations, r.Migrations[0]) },
		"foreign current":      func(r *RegistryV2) { r.CurrentEpochs["power-metadata"]++ },
		"foreign domain":       func(r *RegistryV2) { r.CurrentEpochs["foreign"] = 1; r.MinimumEpochs["foreign"] = 1 },
		"foreign retired digest": func(r *RegistryV2) {
			prefix := r.RetiredPrefixes["settings"]
			prefix.Digest = Fingerprint(strings.Repeat("f", 64))
			r.RetiredPrefixes["settings"] = prefix
		},
	} {
		t.Run(name, func(t *testing.T) {
			registry := retiringCandidate(t)
			change(&registry)
			payload, _ := json.Marshal(registry)
			parsed, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
			if err == nil || digest != "" || !reflect.DeepEqual(parsed, RegistryV2{}) {
				t.Fatalf("foreign registry returned %#v, %s, %v", parsed, digest, err)
			}
		})
	}
	// A matching whole registry is still refused when retired floors are not
	// dependency-closed. No partial retired set may escape.
	unclosed := retire(t, publishedV030Registry(t), map[string]int{"owner-registration": 3, "power-metadata": 3, "settings": 2})
	if ids, err := unclosed.closedRetiredMigrationIDs(); err == nil || ids != nil {
		t.Fatalf("unclosed retirement returned %v, %v", ids, err)
	}
	// The original, open language remains readable strictly, independently
	// of frozen history and without reserving historical IDs in that language.
	strict := singleDomainRegistry()
	strict.Migrations[0].ID = "canonicalize-test-vms-settings-v2"
	strict.RetiredPrefixes = map[string]RetiredMigrationPrefixV2{"settings": {ThroughEpoch: 1, Digest: Fingerprint(strings.Repeat("a", 64))}}
	payload, _ := json.Marshal(strict)
	if _, _, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog()); err != nil {
		t.Fatalf("strict-first registry refused: %v", err)
	}
}

func TestDownPathUsesTargetParserInBothDirections(t *testing.T) {
	base, retired := publishedV030Registry(t), retiringCandidate(t)
	for index, pair := range [][2]RegistryV2{{retired, base}, {retired, retired}} {
		for _, features := range [][]string{nil, {"unknown"}, RegistryReaderFeatures()} {
			path, err := PlanDownPath(pair[0], pair[1], features...)
			payload, _ := json.Marshal(pair[0])
			_, _, parseErr := ParseRegistryV2WithFeatures(payload, BuiltinCapabilityCatalog(), features)
			if path.Relation != []HistoryCompatibilityKind{HistoryEquivalent, HistoryIdentity}[index] {
				t.Fatalf("readability changed underlying relation: %#v", path)
			}
			if err != nil || (path.Unreadable != nil) != (parseErr != nil) {
				t.Fatalf("target features %v: path %#v, %v; parser %v", features, path, err, parseErr)
			}
		}
	}
	// Return from one retired form to a further retirement also needs the
	// selected parser; a successor relation alone is not sufficient.
	partly := retire(t, base, map[string]int{"settings": 2})
	path, err := PlanDownPath(partly, retired)
	if err != nil || !path.Successor || path.Unreadable == nil || path.Kind != HistoryRefuse || path.Relation != HistoryEquivalent {
		t.Fatalf("unaware successor = %#v, %v", path, err)
	}
	path, err = PlanDownPath(partly, retired, RegistryReaderFeatures()...)
	if err != nil || path.Kind != HistoryEquivalent || path.Relation != HistoryEquivalent || !path.Successor {
		t.Fatalf("capable successor = %#v, %v", path, err)
	}
}

func TestShippedRegistryReaderFeaturesMatchCompiledParser(t *testing.T) {
	payload, err := os.ReadFile("../../config/release-rollback.json")
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := ParseRollbackDeclaration(payload)
	if err != nil || !slices.Equal(declaration.RegistryFeatures, RegistryReaderFeatures()) {
		t.Fatalf("sealed features %v, compiled %v, %v", declaration.RegistryFeatures, RegistryReaderFeatures(), err)
	}
	if !maps.Equal(frozenV030Registry.MinimumEpochs, publishedV030Registry(t).MinimumEpochs) {
		t.Fatal("frozen registry changed")
	}
}

func TestUnreadableIdentityKeepsRegistryRelation(t *testing.T) {
	registry := retiringCandidate(t)
	verdict := HistoryCompatibility(HistoryCompatibilityInput{Active: &registry, Target: &registry})
	if verdict.Admitted() || verdict.Registry != RegistryIdentical || verdict.Relation != HistoryIdentity || len(verdict.Refusals) != 1 || verdict.Refusals[0].Reason != HistoryRegistryUnreadable {
		t.Fatalf("unaware identity = %#v", verdict)
	}
}

func TestUnreadablePairsRequireTheDeclarationInTheirDirection(t *testing.T) {
	base, retired := publishedV030Registry(t), retiringCandidate(t)
	partly := retire(t, base, map[string]int{"settings": 2})
	for _, pair := range [][2]RegistryV2{{retired, base}, {partly, retired}} {
		active, _ := pair[0].CanonicalDigest()
		target, _ := pair[1].CanonicalDigest()
		for _, successor := range []bool{false, true} {
			input := HistoryCompatibilityInput{Active: &pair[0], Target: &pair[1]}
			if successor {
				input.TargetPredecessor = active
			} else {
				input.Predecessor = target
			}
			verdict := HistoryCompatibility(input)
			want := 1
			if verdict.Successor != successor {
				want = 2
			}
			if verdict.Admitted() || len(verdict.Refusals) != want || verdict.Refusals[0].Reason != HistoryRegistryUnreadable ||
				want == 2 && verdict.Refusals[1].Reason != HistoryRegistryDiffers {
				t.Fatalf("successor/declaration = %t/%t: %#v", verdict.Successor, successor, verdict)
			}
		}
	}
}

func TestRegistryFallbackPreservesStrictFailure(t *testing.T) {
	registry := publishedV030Registry(t)
	registry.CurrentEpochs["settings"] = 9
	payload, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	_, _, strictErr := ParseRegistryV2WithFeatures(payload, BuiltinCapabilityCatalog(), nil)
	parsed, digest, err := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
	if strictErr == nil || !strings.Contains(strictErr.Error(), "registry path does not end at current epochs") {
		t.Fatalf("strict failure = %v", strictErr)
	}
	if err == nil || !strings.Contains(err.Error(), strictErr.Error()) || digest != "" || !reflect.DeepEqual(parsed, RegistryV2{}) {
		t.Fatalf("default parser lost strict cause %v: %#v, %s, %v", strictErr, parsed, digest, err)
	}
}
