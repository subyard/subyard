package releasetransition

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRollbackRewindRecoversPublishedCheckpointWithoutChangingSettings(t *testing.T) {
	const settings = "YARD_TEMPLATE=test-vms\n"
	home := settingsV2Fixture(t, map[string]string{"yards/example/config.env": settings})
	store, err := NewPOSIXV2Store(home)
	if err != nil {
		t.Fatal(err)
	}
	source := RegistryV2{SchemaVersion: 2, MinimumEpochs: map[string]int{"settings": 1}, CurrentEpochs: map[string]int{"settings": 2}, Migrations: []MigrationDefinitionV2{builtinMigrationDefinitions()[0].migration}}
	target := RegistryV2{SchemaVersion: 2, MinimumEpochs: source.MinimumEpochs, CurrentEpochs: map[string]int{"settings": 1}}
	sourceBytes, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	targetBytes, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	pair := ReleasePair{From: "source", Previous: releaseIDPointer("target"), Target: "target"}
	binding := RollbackSourceOwnerBinding{ConfigHome: home, RuntimeRoot: "/runtime", Releases: pair, SourceArtifact: digestA, TargetArtifact: digestB, SourceRegistry: fingerprintPayload(sourceBytes), TargetRegistry: fingerprintPayload(targetBytes)}
	state := rewindSnapshot(t, source, target)
	origin := rollbackHistoryOrigin{SchemaVersion: 1, Owner: binding, SourceRegistry: sourceBytes, TargetRegistry: targetBytes, Projection: state.ProjectionSnapshot, Checkpoint: state.AuthoritySnapshot}
	originBytes, err := origin.marshal()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	transition := V2Transition{registry: source, registryDigest: binding.SourceRegistry, store: store, options: V2Options{ConfigHome: home, Releases: pair}}
	plan, err := transition.assessRollbackHistory(ctx, origin)
	if err != nil || len(plan.blockers) != 0 || len(plan.edges) != 1 {
		t.Fatalf("assessment = %+v, %v", plan, err)
	}
	items, err := transition.rewindOnlyWork(plan.edges[0], origin.Projection.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	journal := validJournal(pair)
	journal.Goal.Direction, journal.Checkpoint = DirectionActivatePrevious, JournalMigrating
	journal.RegistryDigest, journal.ArtifactDigest = binding.SourceRegistry, binding.TargetArtifact
	ownerPin, err := binding.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	originPin := fingerprintPayload(originBytes)
	journal.Steps = []JournalStep{
		{ID: RollbackSourceOwnerStep, Migration: RollbackSourceOwnerStep, Resource: "rollback.source-owner", Decision: DecisionPreserve, Expected: ownerPin, Desired: ownerPin, Checkpoint: StepVerified},
		{ID: rollbackSourcePlanStep, Migration: rollbackSourcePlanStep, Resource: "rollback.source-plan", Decision: DecisionPreserve, Expected: originPin, Desired: originPin, Checkpoint: StepVerified},
	}
	for i := range journal.Steps {
		step := &journal.Steps[i]
		var recovery Fingerprint
		if step.ID == rollbackSourcePlanStep {
			recovery = originPin
		}
		for _, phase := range []EvidenceCheckpoint{EvidenceCaptured, EvidenceApplied, EvidenceVerified} {
			evidence := evidenceFor(journal, *step, phase, step.Expected, recovery)
			if err := transition.persistEvidence(evidence); err != nil {
				t.Fatal(err)
			}
			step.Evidence = &evidence
		}
	}
	for _, item := range items {
		in := item.intent
		journal.Steps = append(journal.Steps, JournalStep{ID: in.ID, Migration: in.Migration, Resource: in.Resource, Decision: in.Decision, Expected: in.Expected, Desired: in.Desired, Checkpoint: StepIntent})
	}
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	if err := store.CreateRecovery(journal.Transaction, rollbackSourcePlanStep, originBytes); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapLedger(absentProtectedSnapshot(), origin.Projection.Payload); err != nil {
		t.Fatal(err)
	}
	if err := store.createImmutable([]string{"release-transition", "v2"}, migrationLedgerCheckpointName, origin.Checkpoint.Payload); err != nil {
		t.Fatal(err)
	}
	selected := absentProtectedSnapshot()
	if err := transition.persistJournal(&journal, &selected); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	links := ReleaseLinks{Active: pair.From, Previous: pair.Previous}
	if result, err := transition.convergeRollbackRewind(ctx, origin, &journal, &selected, links, true); err != nil || result.Code != "" || journal.Steps[2].Checkpoint != StepIntent {
		t.Fatalf("abort began untouched work: %+v, %v", result, err)
	}
	crash := errors.New("crash after checkpoint CAS")
	transition.options.fault = func(point string) error {
		if point == "after-rollback-ledger-cas" {
			return crash
		}
		return nil
	}
	if _, err := transition.convergeRollbackRewind(ctx, origin, &journal, &selected, links, false); !errors.Is(err, crash) {
		t.Fatalf("expected crash after rewind publication, got %v", err)
	}
	if _, err := store.prepareRollbackAbortCut(selected); err == nil {
		t.Fatal("unrecorded ledger publication established an abort cut")
	}
	journal, err = ParseJournal(selected.Payload)
	if err != nil {
		t.Fatal(err)
	}
	fresh := V2Transition{registry: source, registryDigest: binding.SourceRegistry, store: store, options: V2Options{ConfigHome: home, Releases: pair}}
	if result, err := fresh.convergeRollbackRewind(ctx, origin, &journal, &selected, links, true); err != nil || result.Code != "" {
		t.Fatalf("fresh recovery: %+v, %v", result, err)
	}
	cut, err := store.prepareRollbackAbortCut(selected)
	if err != nil || cut.history.Ledger.Domains["settings"].Epoch != 1 {
		t.Fatalf("verified abort cut: %+v, %v", cut, err)
	}
	if err := verifyCanonicalSettingsMigration(&fresh, ctx); err != nil {
		t.Fatal(err)
	}
}
