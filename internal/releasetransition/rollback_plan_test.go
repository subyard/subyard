package releasetransition

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAssessRollbackHistoryUsesCompiledPolicyAndProjection(t *testing.T) {
	for _, tc := range []struct {
		name               string
		settings           string
		applied, pinSource bool
		wantBlockers       int
	}{
		{name: "canonical rewind", settings: "YARD_TEMPLATE=test-vms\n"},
		{name: "settings drift", settings: "YARD_TEMPLATE=e2e-vms\n", wantBlockers: 1},
		{name: "independent irreversible and projection refusals", settings: "YARD_TEMPLATE=e2e-vms\n", applied: true, pinSource: true, wantBlockers: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const relative = "yards/example/config.env"
			home := settingsV2Fixture(t, map[string]string{relative: tc.settings})
			defs := builtinMigrationDefinitions()
			source := RegistryV2{SchemaVersion: 2,
				MinimumEpochs: map[string]int{"settings": 1, "power-metadata": 1},
				CurrentEpochs: map[string]int{"settings": 2, "power-metadata": 2},
				Migrations:    []MigrationDefinitionV2{defs[0].migration, defs[1].migration}}
			target := RegistryV2{SchemaVersion: 2, MinimumEpochs: source.MinimumEpochs,
				CurrentEpochs: map[string]int{"settings": 1, "power-metadata": 1}}
			initial := completeRewindLedger(source)
			if !tc.applied {
				initial.Domains["power-metadata"] = BaselineLedgerV2(source).Domains["power-metadata"]
			}
			encode := func(value RegistryV2) []byte {
				t.Helper()
				payload, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return payload
			}
			sourceBytes, targetBytes := encode(source), encode(target)
			pair := ReleasePair{From: "source", Previous: releaseIDPointer("target"), Target: "target"}
			binding := RollbackSourceOwnerBinding{ConfigHome: home, RuntimeRoot: "/runtime", Releases: pair,
				SourceArtifact: digestA, TargetArtifact: digestB, SourceRegistry: fingerprintPayload(sourceBytes), TargetRegistry: fingerprintPayload(targetBytes)}
			projectionLedger := BaselineLedgerV2(source)
			epochs := target.CurrentEpochs
			if tc.pinSource {
				projectionLedger, epochs = initial, source.CurrentEpochs
			}
			projectionBytes, _, err := MarshalLedgerV2(projectionLedger, source)
			if err != nil {
				t.Fatal(err)
			}
			projection := protectedSnapshotFromPayload(projectionBytes)
			checkpoint, err := newLedgerCheckpointV1(initial, source, binding.SourceRegistry, projection, epochs)
			if err != nil {
				t.Fatal(err)
			}
			payload, _, err := MarshalLedgerCheckpointV1(checkpoint, source, binding.SourceRegistry, projection)
			if err != nil {
				t.Fatal(err)
			}
			origin := rollbackHistoryOrigin{SchemaVersion: 1, Owner: binding, SourceRegistry: sourceBytes,
				TargetRegistry: targetBytes, Projection: projection, Checkpoint: protectedSnapshotFromPayload(payload)}
			transition := V2Transition{registry: source, registryDigest: binding.SourceRegistry, options: V2Options{ConfigHome: home, Releases: pair}}
			plan, err := transition.assessRollbackHistory(context.Background(), origin)
			if err != nil || len(plan.blockers) != tc.wantBlockers {
				t.Fatalf("assessment refusals = %+v, %v", plan.blockers, err)
			}
			for _, blocker := range plan.blockers {
				if err := blocker.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.wantBlockers == 0 && (len(plan.edges) != 1 || !sameLedgerV2(plan.native.Ledger, BaselineLedgerV2(target)) || len(plan.work) != 0) {
				t.Fatalf("incorrect rewind-only preview: %+v", plan)
			}
			actual, err := os.ReadFile(filepath.Join(home, relative))
			if err != nil || string(actual) != tc.settings {
				t.Fatal("assessment changed configuration")
			}
		})
	}
}
