package releasetransition

import (
	"context"
	"encoding/json"
)

//go:generate go run ./cmd/registry ../../config/release-transition.json

// Authored definitions are the single registration point for Subyard migrations.
// Runtime files select these compiled implementations; they cannot define policy.
type migrationDefinition struct {
	// Catalog generation is compiled release provenance, not registry policy.
	catalogGeneration int
	migration         MigrationDefinitionV2
	capability        CapabilityDescriptor
	recipe            migrationRecipe
	forward           func(*V2Transition, context.Context, *v2Observation, MigrationDefinitionV2, V2SettingsSnapshotView) (V2SettingsSnapshotView, error)
	settings          func(*V2Transition, context.Context, MigrationDefinitionV2, V2SettingsSnapshotView, ReleaseLinks) (settingsV2Plan, error)
	rollback          rollbackPolicy
}

type migrationRecipe string

const (
	canonicalSettingsRecipe   migrationRecipe = "canonical-settings"
	localSSHSettingsRecipe    migrationRecipe = "local-ssh-settings"
	ownerRegistrationRecipe   migrationRecipe = "owner-registration"
	publicationBoundaryRecipe migrationRecipe = "publication-boundary"
	registeredYardsRecipe     migrationRecipe = "registered-yards"
	guestDiskTmpRecipe        migrationRecipe = "guest-disk-tmp"
	nmDeviceGuardRecipe       migrationRecipe = "nm-device-guard"
)

type rollbackClass string

const (
	rollbackRewindOnly   rollbackClass = "rewind-only"
	rollbackIrreversible rollbackClass = "irreversible"
	rollbackReversible   rollbackClass = "reversible"
)

type rollbackPolicy struct {
	class   rollbackClass
	reason  string
	reverse func(*V2Transition, context.Context, MigrationDefinitionV2) ([]v2Work, error)
	// Rewind-only preserves effects. The ordinary forward inspector must verify
	// that they still satisfy its predicate before history can advance again.
	verify func(*V2Transition, context.Context) error
}

func builtinMigrationDefinitions() []migrationDefinition {
	return []migrationDefinition{
		{
			migration:  MigrationDefinitionV2{ID: "canonicalize-test-vms-settings-v2", Domain: "settings", FromEpoch: 1, ToEpoch: 2, Kind: "test-vms-settings-v1-to-v2"},
			capability: CapabilityDescriptor{Kind: "test-vms-settings-v1-to-v2", Domain: "settings", Version: 1, RollbackCompatibility: RollbackCompatibilityBackwardCompatibleNoOp, LegacyMinimumVersion: "0.8.0"},
			recipe:     canonicalSettingsRecipe, forward: planCanonicalSettingsMigration, settings: inspectCanonicalSettingsMigration,
			rollback: rollbackPolicy{class: rollbackRewindOnly, verify: verifyCanonicalSettingsMigration},
		},
		{
			migration:         MigrationDefinitionV2{ID: "introduce-owner-publication-state-v1", Domain: "power-metadata", FromEpoch: 1, ToEpoch: 2, Kind: "owner-publication-state-v1"},
			capability:        CapabilityDescriptor{Kind: "owner-publication-state-v1", Domain: "power-metadata", Version: 1},
			catalogGeneration: 1,
			recipe:            publicationBoundaryRecipe, forward: planPublicationBoundaryMigration,
			rollback: rollbackPolicy{class: rollbackIrreversible, reason: "older owners cannot read detached publication state"},
		},
		{
			migration:  MigrationDefinitionV2{ID: "canonicalize-test-yard-owner-v2", Domain: "owner-registration", FromEpoch: 1, ToEpoch: 2, Kind: "test-yard-owner-v1-to-v2", DependsOn: []string{"canonicalize-test-vms-settings-v2"}},
			capability: CapabilityDescriptor{Kind: "test-yard-owner-v1-to-v2", Domain: "owner-registration", Version: 1, RollbackCompatibility: RollbackCompatibilityBackwardCompatibleNoOp, LegacyMinimumVersion: "0.8.0"},
			recipe:     ownerRegistrationRecipe, forward: planOwnerRegistrationMigration,
			rollback: rollbackPolicy{class: rollbackIrreversible, reason: "native rewind verification is not established for owner registration"},
		},
		{
			migration:         MigrationDefinitionV2{ID: "localize-ssh-port-settings-v3", Domain: "settings", FromEpoch: 2, ToEpoch: 3, Kind: localSSHPortMigration, DependsOn: []string{"canonicalize-test-vms-settings-v2", "canonicalize-test-yard-owner-v2"}},
			capability:        CapabilityDescriptor{Kind: localSSHPortMigration, Domain: "settings", Version: 1},
			catalogGeneration: 1,
			recipe:            localSSHSettingsRecipe, forward: planLocalSSHMigration, settings: inspectLocalSSHMigration,
			rollback: rollbackPolicy{class: rollbackIrreversible, reason: "older readers cannot honor local SSH port ownership"},
		},
		{
			migration:         MigrationDefinitionV2{ID: "register-existing-local-yards-v3", Domain: "owner-registration", FromEpoch: 2, ToEpoch: 3, Kind: registeredYardsMigration, DependsOn: []string{"canonicalize-test-yard-owner-v2", "localize-ssh-port-settings-v3"}},
			capability:        CapabilityDescriptor{Kind: registeredYardsMigration, Domain: "owner-registration", Version: 1},
			catalogGeneration: 2,
			recipe:            registeredYardsRecipe, forward: planNativeMigration, settings: inspectRegisteredMigrationSettings,
			rollback: rollbackPolicy{class: rollbackIrreversible, reason: "native registration has no supported inverse"},
		},
		{
			migration:         MigrationDefinitionV2{ID: "complete-registered-guest-disk-tmp-v3", Domain: "power-metadata", FromEpoch: 2, ToEpoch: 3, Kind: guestDiskTmpMigration, DependsOn: []string{"register-existing-local-yards-v3", "introduce-owner-publication-state-v1"}},
			capability:        CapabilityDescriptor{Kind: guestDiskTmpMigration, Domain: "power-metadata", Version: 1},
			catalogGeneration: 2,
			recipe:            guestDiskTmpRecipe, forward: planNativeMigration,
			rollback: rollbackPolicy{class: rollbackIrreversible, reason: "guest storage migration has no supported inverse"},
		},
		{
			migration:         MigrationDefinitionV2{ID: "introduce-nm-device-guard-v4", Domain: "power-metadata", FromEpoch: 3, ToEpoch: 4, Kind: nmDeviceGuardMigration, DependsOn: []string{"complete-registered-guest-disk-tmp-v3"}},
			capability:        CapabilityDescriptor{Kind: nmDeviceGuardMigration, Domain: "power-metadata", Version: 1},
			catalogGeneration: 3,
			recipe:            nmDeviceGuardRecipe, forward: planNMDeviceGuardMigration,
			rollback: rollbackPolicy{class: rollbackIrreversible, reason: "older power readers cannot honor the NetworkManager device guard"},
		},
	}
}

func compiledMigration(migration MigrationDefinitionV2) (migrationDefinition, bool) {
	var fallback migrationDefinition
	known := false
	for _, definition := range builtinMigrationDefinitions() {
		if definition.capability.Kind == migration.Kind && definition.capability.Domain == migration.Domain {
			if definition.migration.ID == migration.ID {
				return definition, true
			}
			fallback, known = definition, true
		}
	}
	return fallback, known
}

func migrationRecipeFor(migration MigrationDefinitionV2) migrationRecipe {
	definition, _ := compiledMigration(migration)
	return definition.recipe
}

func builtinRollbackPolicies() map[string]rollbackPolicy {
	policies := make(map[string]rollbackPolicy)
	for _, definition := range builtinMigrationDefinitions() {
		policies[definition.migration.ID] = definition.rollback
	}
	return policies
}

// BuiltinRegistryPayload generates the sealed registry without changing its V2
// shape or historical capability digests.
func BuiltinRegistryPayload() ([]byte, error) {
	if err := validateMigrationDefinitions(builtinMigrationDefinitions()); err != nil {
		return nil, err
	}
	registry := RegistryV2{SchemaVersion: RegistrySchemaV2,
		MinimumEpochs: map[string]int{"settings": 1, "project-state": 1, "owner-registration": 1, "power-metadata": 1},
		CurrentEpochs: map[string]int{"settings": 1, "project-state": 1, "owner-registration": 1, "power-metadata": 1},
	}
	for _, definition := range builtinMigrationDefinitions() {
		registry.Migrations = append(registry.Migrations, definition.migration)
		registry.CurrentEpochs[definition.migration.Domain] = definition.migration.ToEpoch
	}
	if err := registry.Validate(BuiltinCapabilityCatalog()); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(registry, "", "  ")
	return append(payload, '\n'), err
}

// MigrationSourceDecisionsComplete describes the existing source inspection
// contract. Native recipes retain their separately negotiated activation path.
func MigrationSourceDecisionsComplete(kind string) bool {
	for _, definition := range builtinMigrationDefinitions() {
		if definition.capability.Kind == kind {
			return definition.recipe != registeredYardsRecipe && definition.recipe != guestDiskTmpRecipe
		}
	}
	return false
}

func validateMigrationDefinitions(definitions []migrationDefinition) error {
	ids := map[string]bool{}
	kinds := map[string]CapabilityDescriptor{}
	generations := map[string]int{}
	for _, definition := range definitions {
		migration, capability, policy := definition.migration, definition.capability, definition.rollback
		if definition.catalogGeneration < 0 || ids[migration.ID] || migration.Kind != capability.Kind || migration.Domain != capability.Domain || definition.forward == nil || definition.recipe == "" {
			return invalid("migration %q has an incomplete or duplicate compiled definition", migration.ID)
		}
		if previous, exists := kinds[capability.Kind]; exists && (previous != capability || generations[capability.Kind] != definition.catalogGeneration) {
			return invalid("migration %q changes an existing compiled capability", migration.ID)
		}
		ids[migration.ID], kinds[capability.Kind] = true, capability
		generations[capability.Kind] = definition.catalogGeneration
		switch policy.class {
		case rollbackRewindOnly:
			// The frozen descriptor remains the legacy compatibility assertion. Native
			// rewind additionally requires this migration's explicit state verifier.
			if capability.RollbackCompatibility != RollbackCompatibilityBackwardCompatibleNoOp || policy.verify == nil || policy.reverse != nil || policy.reason != "" {
				return invalid("migration %q lacks a compatible rewind-only verifier", migration.ID)
			}
		case rollbackReversible:
			if policy.reverse == nil || policy.verify == nil || policy.reason != "" {
				return invalid("migration %q lacks a typed inverse and verifier", migration.ID)
			}
		case rollbackIrreversible:
			// A legacy no-op declaration is not a promise of native rewind. Keeping a
			// conservative policy here leaves historical descriptors unchanged.
			if policy.reason == "" || policy.reverse != nil || policy.verify != nil {
				return invalid("migration %q has an invalid irreversible policy", migration.ID)
			}
		default:
			return invalid("migration %q has no compiled rollback policy", migration.ID)
		}
	}
	return nil
}
