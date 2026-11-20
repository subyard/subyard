package releasetransition

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// v030RegistryDependencies is the migration loop of migrationhistory.NewRegistry
// at v0.30.0 (tag commit e2b31943, internal/migrationhistory/history.go:87-112),
// without its ID syntax checks: each
// dependency must be a migration defined earlier in the same registry. The
// v0.30.0 engine verb applies it to the active release's registry through
// CheckSourcePolicy before it serves any request, a rollback included.
func v030RegistryDependencies(migrations []MigrationDefinitionV2) error {
	ids := map[string]int{}
	for i, m := range migrations {
		if _, ok := ids[m.ID]; ok {
			return fmt.Errorf("duplicate migration ID %q", m.ID)
		}
		seen := map[string]bool{}
		for _, dep := range m.DependsOn {
			if seen[dep] {
				return fmt.Errorf("migration %q has invalid or duplicate dependency %q", m.ID, dep)
			}
			if _, ok := ids[dep]; !ok {
				return fmt.Errorf("migration %q dependency %q is unknown, cyclic or not ordered before it", m.ID, dep)
			}
			seen[dep] = true
		}
		ids[m.ID] = i
	}
	return nil
}

// Every retirement of the v0.30.0 registry is Equivalent to it exactly when the
// v0.30.0 dependency rule accepts the retired registry. Only the retirements
// that also retire the NM guard step pass, so no successor can keep that step
// live, retire real history and still roll back to v0.30.0.
func TestEquivalentActiveRegistriesSatisfyTheV030DependencyRule(t *testing.T) {
	v030 := publishedV030Registry(t)
	if err := v030RegistryDependencies(v030.Migrations); err != nil {
		t.Fatal(err)
	}
	candidate := retiringCandidate(t)
	if path, _ := PlanDownPath(candidate, v030); v030RegistryDependencies(candidate.Migrations) == nil || path.Kind != HistoryRefuse {
		t.Fatalf("retiring candidate: v0.30.0 rule accepts it or the down path %#v admits it", path)
	}
	live := 0
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
					retired := retire(t, v030, floors)
					path, err := PlanDownPath(retired, v030)
					if err != nil {
						t.Fatal(err)
					}
					accepted := v030RegistryDependencies(retired.Migrations) == nil
					if (path.Kind == HistoryEquivalent) != accepted {
						t.Errorf("floors %v: down path %s, v0.30.0 accepts %v", floors, path.Kind, accepted)
					}
					if accepted && power <= 3 {
						live++
						if owner != 1 || power != 1 || settings != 1 {
							t.Errorf("floors %v keep the NM guard step live and retire definitions", floors)
						}
					}
				}
			}
		}
	}
	if live != 1 {
		t.Fatalf("%d retirements keep the NM guard step live; only project-state should", live)
	}
}

// A target that declares RegistryFeatureRetiredPrefixDependencies reads
// dependencies on proved retired definitions, so every retirement of the
// v0.30.0 registry is Equivalent for it exactly when this release's own parser
// accepts the retired registry. A target without the feature keeps the
// v0.30.0 rule.
func TestFeatureDeclaringTargetReadsProvedRetiredDependencies(t *testing.T) {
	v030 := publishedV030Registry(t)
	feature := RegistryFeatureRetiredPrefixDependencies
	candidate := retiringCandidate(t)
	if path, err := PlanDownPath(candidate, v030, feature); err != nil || path.Kind != HistoryEquivalent || path.Unreadable != nil {
		t.Fatalf("retiring candidate for a feature-declaring target: %#v, %v", path, err)
	}
	if path, err := PlanDownPath(candidate, v030, "a-later-form"); err != nil || path.Kind != HistoryRefuse || path.Unreadable == nil {
		t.Fatalf("retiring candidate for a target with only unknown features: %#v, %v", path, err)
	}
	for owner := 1; owner <= 3; owner++ {
		for power := 1; power <= 4; power++ {
			for settings := 1; settings <= 3; settings++ {
				floors := map[string]int{}
				for domain, floor := range map[string]int{"owner-registration": owner, "power-metadata": power, "settings": settings} {
					if floor > 1 {
						floors[domain] = floor
					}
				}
				if len(floors) == 0 {
					continue
				}
				retired := retire(t, v030, floors)
				path, err := PlanDownPath(retired, v030, feature)
				if err != nil {
					t.Fatal(err)
				}
				payload, err := json.Marshal(retired)
				if err != nil {
					t.Fatal(err)
				}
				_, _, parseErr := ParseRegistryV2(payload, BuiltinCapabilityCatalog())
				if (path.Kind == HistoryEquivalent) != (parseErr == nil) {
					t.Errorf("floors %v: down path %s, this release parses it: %v", floors, path.Kind, parseErr)
				}
			}
		}
	}
	// The verdict admits the declared predecessor only for a feature-declaring target.
	predecessor, err := v030.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		features []string
		admitted bool
	}{{[]string{feature}, true}, {nil, false}} {
		verdict := HistoryCompatibility(HistoryCompatibilityInput{
			Active: &candidate, Target: &v030, Predecessor: predecessor, TargetRegistryFeatures: test.features,
		})
		if verdict.Admitted() != test.admitted || test.admitted != (verdict.Kind == HistoryEquivalent) ||
			!test.admitted && !slices.ContainsFunc(verdict.Refusals, func(refusal HistoryRefusal) bool { return refusal.Reason == HistoryRegistryUnreadable }) {
			t.Errorf("features %v: verdict %#v", test.features, verdict)
		}
	}
}
