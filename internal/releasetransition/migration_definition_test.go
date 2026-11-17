package releasetransition

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestAuthoredMigrationsExactlyCoverRegistryAndRollbackPolicy(t *testing.T) {
	payload, err := BuiltinRegistryPayload()
	if err != nil {
		t.Fatal(err)
	}
	published, err := os.ReadFile(filepath.Join("..", "..", "config", "release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, published) {
		t.Fatal("release registry differs from compiled definitions; run go generate ./internal/releasetransition")
	}
	registry, _, err := ParseRegistryV2(published, BuiltinCapabilityCatalog())
	if err != nil {
		t.Fatal(err)
	}
	policies := builtinRollbackPolicies()
	if len(policies) != len(registry.Migrations) {
		t.Fatal("rollback policy does not exactly cover registry")
	}
	for _, migration := range registry.Migrations {
		if _, exists := policies[migration.ID]; !exists {
			t.Fatalf("missing rollback policy for %s", migration.ID)
		}
	}
	// Catalog bytes from the published six-kind implementation remain frozen.
	legacy := preRegisteredYardsCapabilityCatalog()
	descriptors := legacy.Descriptors()
	descriptors = append(descriptors,
		CapabilityDescriptor{Kind: registeredYardsMigration, Domain: "owner-registration", Version: 1},
		CapabilityDescriptor{Kind: guestDiskTmpMigration, Domain: "power-metadata", Version: 1})
	frozen, err := NewCapabilityCatalog(descriptors)
	if err != nil || frozen.Digest() != registeredYardsCapabilityCatalog().Digest() {
		t.Fatalf("historical catalog identity changed: %v", err)
	}
}

func TestAuthoredMigrationsRejectIncompletePolicies(t *testing.T) {
	for _, change := range []string{"missing", "missing verifier", "missing inverse", "contradictory descriptor", "duplicate", "missing planner", "negative catalog generation", "changed catalog generation"} {
		t.Run(change, func(t *testing.T) {
			definitions := builtinMigrationDefinitions()
			switch change {
			case "missing":
				definitions[0].rollback = rollbackPolicy{}
			case "missing verifier":
				definitions[0].rollback.verify = nil
			case "missing inverse":
				definitions[0].rollback.class = rollbackReversible
			case "contradictory descriptor":
				definitions[0].capability.RollbackCompatibility = ""
			case "duplicate":
				definitions = append(definitions, definitions[0])
			case "missing planner":
				definitions[0].forward = nil
			case "negative catalog generation":
				definitions[0].catalogGeneration = -1
			case "changed catalog generation":
				another := definitions[0]
				another.migration.ID = "another-settings-migration"
				another.catalogGeneration = 1
				definitions = append(definitions, another)
			}
			if err := validateMigrationDefinitions(definitions); err == nil {
				t.Fatal("incomplete compiled definition accepted")
			}
		})
	}
}

func TestAuthoredCatalogCheckpointsSurviveANewMigrationKind(t *testing.T) {
	const frozenR = Fingerprint("022b6400661c2d277c77712122f6e40e29644de32ae5cb3de2fe1240d1f5fc63")
	definitions := builtinMigrationDefinitions()
	if got := authoredCatalogThrough(definitions, 3).Digest(); got != frozenR {
		t.Fatalf("the seven-kind catalog checkpoint changed: %s", got)
	}
	future := definitions[0]
	future.migration.ID, future.migration.Kind = "future-fixture-migration", "future-fixture-kind"
	future.capability = CapabilityDescriptor{Kind: future.migration.Kind, Domain: future.migration.Domain, Version: 1}
	future.rollback = rollbackPolicy{class: rollbackIrreversible, reason: "fixture only"}
	future.catalogGeneration = 4
	definitions = append(definitions, future)
	if err := validateMigrationDefinitions(definitions); err != nil {
		t.Fatal(err)
	}
	current := authoredCatalogThrough(definitions, -1)
	if current.Digest() == frozenR || !current.Supports(future.migration.Kind, future.migration.Domain) {
		t.Fatal("the new authored kind did not enter the successor catalog")
	}
	original, supported := supportedAuthoredCatalog(definitions, frozenR)
	if !supported || original.Digest() != frozenR || original.Supports(future.migration.Kind, future.migration.Domain) {
		t.Fatal("adding a kind changed or lost the retained owner's exact catalog")
	}
}

func TestAuthoredSettingsMigrationReverifiesCanonicalState(t *testing.T) {
	transition, _, path := v2TransitionFixture(t, nil)
	ctx := context.Background()
	goal := Goal{Target: "release-a", Direction: DirectionActivateTarget}
	inspection, err := transition.Inspect(ctx, goal)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := transition.Converge(ctx, Execution{Plan: inspection.Plan, Authorization: v2TestAuthorization(inspection.Plan)})
	if err != nil || outcome.Status != StatusReady {
		t.Fatalf("forward migration = %+v, %v", outcome, err)
	}
	definition := builtinMigrationDefinitions()[0]
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := definition.rollback.verify(transition, ctx); err != nil {
		t.Fatal(err)
	}
	// Re-running the actual forward planner on retained canonical effects must
	// observe the state and produce no resource mutation, even when pending again.
	observation := v2Observation{}
	if _, err := definition.forward(transition, ctx, &observation, definition.migration, nil); err != nil {
		t.Fatal(err)
	}
	if len(observation.work) != 0 || len(observation.blockers) != 0 {
		t.Fatalf("canonical state replayed effects: %+v", observation)
	}
	after, err := os.ReadFile(path)
	finalInfo, statErr := os.Stat(path)
	if err != nil || statErr != nil || !bytes.Equal(before, after) || !os.SameFile(info, finalInfo) || !info.ModTime().Equal(finalInfo.ModTime()) {
		t.Fatalf("verification changed canonical settings: %v, %v", err, statErr)
	}
	testkit.WriteFile(t, path, []byte("YARD_TEMPLATE=e2e-vms\nNESTED_E2E_VMS=1\n"), 0o600)
	if err := definition.rollback.verify(transition, ctx); err == nil {
		t.Fatal("noncanonical state accepted as a verified no-op")
	}
}
