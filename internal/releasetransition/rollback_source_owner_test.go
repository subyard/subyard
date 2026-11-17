package releasetransition

import "testing"

func rollbackOwnerFixtureStep(t *testing.T, request RollbackTargetRequest) JournalStep {
	t.Helper()
	owner := rollbackRequestSourceOwner(request)
	pin, err := owner.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return JournalStep{ID: RollbackSourceOwnerStep, Migration: RollbackSourceOwnerStep, Resource: "rollback.source-owner", Decision: DecisionPreserve, Expected: pin, Desired: pin, Checkpoint: StepVerified}
}

func TestRollbackSourceOwnerPinsExactImageBeforeAnyWork(t *testing.T) {
	binding := RollbackSourceOwnerBinding{RuntimeRoot: "/runtime", ConfigHome: "/config", Releases: ReleasePair{From: "source", Target: "target"}, SourceArtifact: digestA, SourceRegistry: digestB, TargetArtifact: digestC, TargetRegistry: digestD}
	pin, err := binding.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if pin != "3fe747e7cbfaab511ca0f345f974cd8135380afbccfc4c273f97064fe504af8d" {
		t.Fatalf("the source-owner contract fingerprint changed: %s", pin)
	}
	journal := validJournal(binding.Releases)
	journal.Goal.Direction = DirectionActivatePrevious
	journal.ArtifactDigest = binding.TargetArtifact
	journal.RegistryDigest = binding.SourceRegistry
	journal.Steps = []JournalStep{{ID: RollbackSourceOwnerStep, Migration: RollbackSourceOwnerStep, Resource: "rollback.source-owner", Decision: DecisionPreserve, Expected: pin, Desired: pin, Checkpoint: StepIntent}}
	journal.IntentDigest = bindJournalIntent(journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps)
	if !HasRollbackSourceOwner(journal) || binding.CheckJournal(journal) != nil {
		t.Fatal("an authorized source owner cannot be identified before its first effect")
	}
	for _, change := range []string{"image", "source registry", "target image", "target registry", "runtime root", "config home", "source identity", "target identity"} {
		t.Run(change, func(t *testing.T) {
			other := binding
			switch change {
			case "image":
				other.SourceArtifact = digestD
			case "source registry":
				other.SourceRegistry = digestA
			case "target image":
				other.TargetArtifact = digestA
			case "target registry":
				other.TargetRegistry = digestA
			case "runtime root":
				other.RuntimeRoot = "/other-runtime"
			case "config home":
				other.ConfigHome = "/other-config"
			case "source identity":
				other.Releases.From = "different-source"
			case "target identity":
				other.Releases.Target = "different-target"
			}
			if other.CheckJournal(journal) == nil {
				t.Fatal("a different source owner acquired the authorized journal")
			}
		})
	}
}
