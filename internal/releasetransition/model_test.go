package releasetransition

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	digestA Fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB Fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC Fingerprint = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	digestD Fingerprint = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func TestEvaluateMapsEveryDurableCheckpointFromObservedFacts(t *testing.T) {
	pair := releasePair()
	initial := initialLinks(pair)
	activated := activatedLinks(pair)
	sameRelease := ReleasePair{From: pair.Target, Previous: releasePtr("1.0.0"), Target: pair.Target}
	completeWithoutFixedPoint := factsWithJournal(
		pair, activated, JournalComplete, StepVerified, digestC,
	)
	completeWithoutFixedPoint.FixedPointVerified = false

	tests := []struct {
		name          string
		facts         TransitionFacts
		wantStatus    PublicStatus
		wantCode      OutcomeCode
		wantActive    ReleaseID
		wantReached   bool
		wantSafeRetry bool
	}{
		{
			name:       "unstarted transition needs migration and keeps the old runtime active",
			facts:      TransitionFacts{Goal: targetGoal(pair), Releases: pair, Links: initial},
			wantStatus: StatusMigrationRequired, wantCode: CodeTransitionRequired,
			wantActive: pair.From, wantSafeRetry: true,
		},
		{
			name:       "active target without completion evidence fails closed",
			facts:      TransitionFacts{Goal: targetGoal(pair), Releases: pair, Links: activated},
			wantStatus: StatusOperatorActionRequired, wantCode: CodeRecoveryAmbiguous,
			wantActive: pair.Target, wantSafeRetry: true,
		},
		{
			name: "active target with a trusted verified fixed point is ready after journal GC",
			facts: TransitionFacts{
				Goal: targetGoal(pair), Releases: pair, Links: activated, FixedPointVerified: true,
			},
			wantStatus: StatusReady, wantCode: CodeReady,
			wantActive: pair.Target, wantReached: true,
		},
		{
			name: "same-release goal without a verified fixed point remains pending",
			facts: TransitionFacts{
				Goal: targetGoal(sameRelease), Releases: sameRelease, Links: initialLinks(sameRelease),
			},
			wantStatus: StatusMigrationRequired, wantCode: CodeTransitionRequired,
			wantActive: pair.Target, wantSafeRetry: true,
		},
		{
			name: "same-release goal with a verified fixed point is ready",
			facts: TransitionFacts{
				Goal: targetGoal(sameRelease), Releases: sameRelease, Links: initialLinks(sameRelease),
				FixedPointVerified: true,
			},
			wantStatus: StatusReady, wantCode: CodeReady,
			wantActive: pair.Target, wantReached: true,
		},
		{
			name:       "authorized journal resumes from expected-before",
			facts:      factsWithJournal(pair, initial, JournalAuthorized, StepIntent, digestA),
			wantStatus: StatusRecovering, wantCode: CodeRecoveryPending,
			wantActive: pair.From, wantSafeRetry: true,
		},
		{
			name:       "migration intent resumes from desired-after",
			facts:      factsWithJournal(pair, initial, JournalMigrating, StepIntent, digestB),
			wantStatus: StatusRecovering, wantCode: CodeRecoveryPending,
			wantActive: pair.From, wantSafeRetry: true,
		},
		{
			name:       "captured evidence resumes",
			facts:      factsWithJournal(pair, initial, JournalMigrating, StepEvidence, digestA),
			wantStatus: StatusRecovering, wantCode: CodeRecoveryPending,
			wantActive: pair.From, wantSafeRetry: true,
		},
		{
			name:       "applied checkpoint re-observes expected-before and safely resumes",
			facts:      factsWithJournal(pair, initial, JournalMigrating, StepApplied, digestA),
			wantStatus: StatusRecovering, wantCode: CodeRecoveryPending,
			wantActive: pair.From, wantSafeRetry: true,
		},
		{
			name:       "verified migration proceeds to activation",
			facts:      factsWithJournal(pair, initial, JournalMigrating, StepVerified, digestC),
			wantStatus: StatusRecovering, wantCode: CodeRecoveryPending,
			wantActive: pair.From, wantSafeRetry: true,
		},
		{
			name:       "activation intent before the link switch keeps the old runtime",
			facts:      factsWithJournal(pair, initial, JournalActivationIntent, StepVerified, digestC),
			wantStatus: StatusRecovering, wantCode: CodeRecoveryPending,
			wantActive: pair.From, wantSafeRetry: true,
		},
		{
			name:       "activation intent after the link switch recovers forward",
			facts:      factsWithJournal(pair, activated, JournalActivationIntent, StepVerified, digestC),
			wantStatus: StatusRecovering, wantCode: CodeRecoveryPending,
			wantActive: pair.Target, wantSafeRetry: true,
		},
		{
			name:       "target-active checkpoint recovers forward",
			facts:      factsWithJournal(pair, activated, JournalTargetActive, StepVerified, digestC),
			wantStatus: StatusRecovering, wantCode: CodeRecoveryPending,
			wantActive: pair.Target, wantSafeRetry: true,
		},
		{
			name:       "reconcile checkpoint recovers forward",
			facts:      factsWithJournal(pair, activated, JournalReconciling, StepVerified, digestC),
			wantStatus: StatusRecovering, wantCode: CodeRecoveryPending,
			wantActive: pair.Target, wantSafeRetry: true,
		},
		{
			name:       "completed one-time migration stays ready despite later resource drift",
			facts:      factsWithJournal(pair, activated, JournalComplete, StepVerified, digestC),
			wantStatus: StatusReady, wantCode: CodeReady,
			wantActive: pair.Target, wantReached: true,
		},
		{
			name:       "terminal journal without a current verified fixed point recovers",
			facts:      completeWithoutFixedPoint,
			wantStatus: StatusRecovering, wantCode: CodeRecoveryPending,
			wantActive: pair.Target, wantSafeRetry: true,
		},
		{
			name: "same-release authorized transition completes without switching links",
			facts: factsWithJournal(
				sameRelease, initialLinks(sameRelease), JournalComplete, StepVerified, digestC,
			),
			wantStatus: StatusReady, wantCode: CodeReady,
			wantActive: pair.Target, wantReached: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Evaluate(test.facts)
			if got.Status != test.wantStatus || got.Code != test.wantCode ||
				got.Active != test.wantActive || got.ReachedGoal != test.wantReached ||
				got.Target != pair.Target {
				t.Fatalf("Evaluate() = %#v", got)
			}
			if (got.Retry != "") != test.wantSafeRetry {
				t.Fatalf("Evaluate().Retry = %q, wantSafeRetry=%t", got.Retry, test.wantSafeRetry)
			}
			if got.Retry != "" && !isOneSafeAction(got.Retry) {
				t.Fatalf("Evaluate().Retry = %q, want exactly one safe action", got.Retry)
			}
		})
	}
}

func TestEvaluateFailsClosedForForeignAmbiguousOrUnknownFacts(t *testing.T) {
	pair := releasePair()
	foreign := factsWithJournal(pair, initialLinks(pair), JournalAuthorized, StepIntent, digestA)
	foreign.Journal.Releases.Target = "3.0.0"

	unknownJournal := factsWithJournal(pair, initialLinks(pair), JournalAuthorized, StepIntent, digestA)
	unknownJournal.Journal.Checkpoint = JournalCheckpoint("foreign")

	staleResource := factsWithJournal(pair, initialLinks(pair), JournalMigrating, StepEvidence, digestC)

	extraResource := factsWithJournal(pair, initialLinks(pair), JournalMigrating, StepIntent, digestA)
	extraResource.Steps = append(extraResource.Steps, StepObservation{Step: "foreign-step", Fingerprint: digestA})

	wrongLinks := initialLinks(pair)
	wrongLinks.Previous = releasePtr("0.8.0")

	journalBeforeIntentButTargetActive := factsWithJournal(
		pair, activatedLinks(pair), JournalMigrating, StepVerified, digestC,
	)

	completeButOldActive := factsWithJournal(
		pair, initialLinks(pair), JournalComplete, StepVerified, digestC,
	)

	tests := []struct {
		name     string
		facts    TransitionFacts
		wantCode OutcomeCode
	}{
		{"foreign journal", foreign, CodeJournalInvalid},
		{"unknown journal checkpoint", unknownJournal, CodeJournalInvalid},
		{"third actual resource state", staleResource, CodeMigrationStale},
		{"foreign actual resource", extraResource, CodeRecoveryAmbiguous},
		{"unknown link pair", TransitionFacts{Goal: targetGoal(pair), Releases: pair, Links: wrongLinks}, CodeActivationAmbiguous},
		{"target active without activation intent", journalBeforeIntentButTargetActive, CodeActivationAmbiguous},
		{"completed journal with old runtime active", completeButOldActive, CodeActivationAmbiguous},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Evaluate(test.facts)
			if got.Status != StatusOperatorActionRequired || got.Code != test.wantCode ||
				got.Retry == "" || !isOneSafeAction(got.Retry) {
				t.Fatalf("Evaluate() = %#v", got)
			}
		})
	}
}

func TestEvaluateStepCheckpointAndActualStateCartesianMatrix(t *testing.T) {
	pair := releasePair()
	checkpoints := []StepCheckpoint{StepIntent, StepEvidence, StepApplied, StepVerified}
	actuals := []struct {
		name        string
		fingerprint Fingerprint
	}{
		{"expected", digestA},
		{"desired", digestB},
		{"third", digestC},
	}
	for _, checkpoint := range checkpoints {
		for _, actual := range actuals {
			t.Run(string(checkpoint)+"/"+actual.name, func(t *testing.T) {
				facts := factsWithJournal(
					pair, initialLinks(pair), JournalMigrating, checkpoint, actual.fingerprint,
				)
				got := Evaluate(facts)
				if checkpoint != StepVerified && actual.name == "third" {
					if got.Status != StatusOperatorActionRequired || got.Code != CodeMigrationStale {
						t.Fatalf("Evaluate() = %#v, want migration-stale", got)
					}
					return
				}
				if got.Status != StatusRecovering || got.Code != CodeRecoveryPending {
					t.Fatalf("Evaluate() = %#v, want recovering", got)
				}
			})
		}
	}
}

func TestEvaluateJournalCheckpointAndLinkPairLegalityMatrix(t *testing.T) {
	pair := releasePair()
	checkpoints := []JournalCheckpoint{
		JournalAuthorized, JournalMigrating, JournalActivationIntent,
		JournalTargetActive, JournalReconciling, JournalComplete,
	}
	links := []struct {
		name  string
		value ReleaseLinks
	}{
		{"initial", initialLinks(pair)},
		{"staged", ReleaseLinks{Active: pair.From, Previous: releasePtr(pair.Target)}},
		{"activated", activatedLinks(pair)},
	}
	allowed := map[JournalCheckpoint]map[string]bool{
		JournalAuthorized:       {"initial": true},
		JournalMigrating:        {"initial": true},
		JournalActivationIntent: {"initial": true, "staged": true, "activated": true},
		JournalTargetActive:     {"activated": true},
		JournalReconciling:      {"activated": true},
		JournalComplete:         {"activated": true},
	}
	for _, checkpoint := range checkpoints {
		for _, link := range links {
			t.Run(string(checkpoint)+"/"+link.name, func(t *testing.T) {
				facts := factsWithJournal(
					pair, link.value, checkpoint, StepVerified, digestC,
				)
				got := Evaluate(facts)
				if !allowed[checkpoint][link.name] {
					if got.Status != StatusOperatorActionRequired || got.Code != CodeActivationAmbiguous {
						t.Fatalf("Evaluate() = %#v, want activation-ambiguous", got)
					}
					return
				}
				if checkpoint == JournalComplete {
					if got.Status != StatusReady || got.Code != CodeReady {
						t.Fatalf("Evaluate() = %#v, want ready", got)
					}
					return
				}
				if got.Status != StatusRecovering || got.Code != CodeRecoveryPending {
					t.Fatalf("Evaluate() = %#v, want recovering", got)
				}
			})
		}
	}
}

func TestEvaluateRejectsStaleExactResumeBindings(t *testing.T) {
	pair := releasePair()
	tests := map[string]func(*TransitionFacts){
		"plan": func(facts *TransitionFacts) {
			facts.CurrentPlan = PlanToken("resume-v1-" + strings.Repeat("c", 64))
		},
		"artifact": func(facts *TransitionFacts) { facts.CurrentArtifactDigest = digestB },
		"registry": func(facts *TransitionFacts) { facts.CurrentRegistryDigest = digestC },
		"catalog":  func(facts *TransitionFacts) { facts.CurrentCatalogDigest = digestA },
		"authorization grant": func(facts *TransitionFacts) {
			facts.CurrentAuthorizationDigest = digestB
		},
		"verified authorization plan": func(facts *TransitionFacts) {
			facts.VerifiedAuthorizationPlan = PlanToken("plan-v1-" + strings.Repeat("c", 64))
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			facts := factsWithJournal(pair, initialLinks(pair), JournalAuthorized, StepIntent, digestA)
			change(&facts)
			got := Evaluate(facts)
			if got.Status != StatusOperatorActionRequired || got.Code != CodePlanStale ||
				got.Retry == "" {
				t.Fatalf("Evaluate(stale %s) = %#v", name, got)
			}
		})
	}
}

func TestEvaluateRejectsReboundJournalWhenVerifiedAuthorizationPlanIsUnchanged(t *testing.T) {
	pair := releasePair()
	facts := factsWithJournal(pair, initialLinks(pair), JournalAuthorized, StepIntent, digestA)
	verified := facts.VerifiedAuthorizationPlan
	facts.Journal.AuthorizationPlan = PlanToken("plan-v1-" + strings.Repeat("c", 64))
	facts.Journal.ResumePlan = PlanToken("resume-v1-" + strings.Repeat("d", 64))
	facts.Journal.IntentDigest = bindJournalIntent(
		facts.Journal.AuthorizationPlan, facts.Journal.ResumePlan,
		facts.Journal.ObservationScope, facts.Journal.Steps,
	)
	facts.CurrentPlan = facts.Journal.ResumePlan
	facts.VerifiedAuthorizationPlan = verified

	got := Evaluate(facts)
	if got.Status != StatusOperatorActionRequired || got.Code != CodePlanStale || got.Retry == "" {
		t.Fatalf("Evaluate(rebound authorization) = %#v", got)
	}
}

func TestEvaluateRejectsChangedJournalIntentUnderUnchangedCurrentPlan(t *testing.T) {
	tests := map[string]func(*JournalStep){
		"decision":        func(step *JournalStep) { step.Decision = DecisionCanonicalize },
		"expected recipe": func(step *JournalStep) { step.Expected = digestC },
		"desired recipe":  func(step *JournalStep) { step.Desired = digestC },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			pair := releasePair()
			facts := factsWithJournal(
				pair, initialLinks(pair), JournalAuthorized, StepIntent, digestA,
			)
			change(&facts.Journal.Steps[0])
			facts.Journal.IntentDigest = bindJournalIntent(
				facts.Journal.AuthorizationPlan, facts.Journal.ResumePlan,
				facts.Journal.ObservationScope, facts.Journal.Steps,
			)

			got := Evaluate(facts)
			if got.Status != StatusOperatorActionRequired || got.Code != CodePlanStale ||
				got.Retry == "" {
				t.Fatalf("Evaluate(changed %s) = %#v", name, got)
			}
		})
	}
}

func TestEvaluateDoesNotEchoInvalidRawGoalOrLinkValues(t *testing.T) {
	pair := releasePair()
	tests := []TransitionFacts{
		{
			Goal:     Goal{Target: "../raw-goal", Direction: DirectionActivateTarget},
			Releases: pair, Links: initialLinks(pair),
		},
		{
			Goal: targetGoal(pair), Releases: pair,
			Links: ReleaseLinks{Active: "../raw-active", Previous: releasePtr("0.9.0")},
		},
		{
			Goal: targetGoal(pair), Releases: pair,
			Links: ReleaseLinks{Active: pair.From, Previous: releasePtr("../raw-previous")},
		},
	}
	for _, facts := range tests {
		got := Evaluate(facts)
		payload, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if got.Active != "" || got.Previous != nil || got.Target != "" ||
			strings.Contains(string(payload), "../raw-") {
			t.Fatalf("Evaluate(invalid raw facts) leaked %#v as %s", got, payload)
		}
	}
}

func factsWithJournal(
	pair ReleasePair,
	links ReleaseLinks,
	journalCheckpoint JournalCheckpoint,
	stepCheckpoint StepCheckpoint,
	actual Fingerprint,
) TransitionFacts {
	journal := validJournal(pair)
	journal.Checkpoint = journalCheckpoint
	journal.Steps[0].Checkpoint = stepCheckpoint
	switch stepCheckpoint {
	case StepIntent:
		journal.Steps[0].Evidence = nil
	case StepEvidence:
		evidence := validEvidence(pair, EvidenceCaptured, digestA)
		journal.Steps[0].Evidence = &evidence
	case StepApplied:
		evidence := validEvidence(pair, EvidenceApplied, digestB)
		journal.Steps[0].Evidence = &evidence
	case StepVerified:
		evidence := validEvidence(pair, EvidenceVerified, digestB)
		journal.Steps[0].Evidence = &evidence
	}
	return TransitionFacts{
		Goal: targetGoal(pair), Releases: pair, Links: links, Journal: &journal,
		Steps:                      []StepObservation{{Step: "settings-v2", Fingerprint: actual}},
		FixedPointVerified:         journalCheckpoint == JournalComplete,
		CurrentPlan:                journal.ResumePlan,
		CurrentIntents:             plannerIntentsFromJournal(journal.Steps),
		CurrentArtifactDigest:      journal.ArtifactDigest,
		CurrentRegistryDigest:      journal.RegistryDigest,
		CurrentCatalogDigest:       journal.CatalogDigest,
		CurrentAuthorizationDigest: journal.AuthorizationDigest,
		VerifiedAuthorizationPlan:  journal.AuthorizationPlan,
	}
}

func plannerIntentsFromJournal(steps []JournalStep) []PlannerStepIntent {
	intents := make([]PlannerStepIntent, len(steps))
	for index, step := range steps {
		intents[index] = PlannerStepIntent{
			ID: step.ID, Migration: step.Migration, Resource: step.Resource,
			Decision: step.Decision, Expected: step.Expected, Desired: step.Desired,
		}
	}
	return intents
}

func releasePair() ReleasePair {
	return ReleasePair{From: "1.0.0", Previous: releasePtr("0.9.0"), Target: "2.0.0"}
}

func targetGoal(pair ReleasePair) Goal {
	return Goal{Target: pair.Target, Direction: DirectionActivateTarget}
}

func initialLinks(pair ReleasePair) ReleaseLinks {
	return ReleaseLinks{Active: pair.From, Previous: cloneReleasePtr(pair.Previous)}
}

func activatedLinks(pair ReleasePair) ReleaseLinks {
	return ReleaseLinks{Active: pair.Target, Previous: releasePtr(pair.From)}
}

func releasePtr(value ReleaseID) *ReleaseID {
	copy := value
	return &copy
}

func cloneReleasePtr(value *ReleaseID) *ReleaseID {
	if value == nil {
		return nil
	}
	return releasePtr(*value)
}

func isOneSafeAction(action string) bool {
	return strings.TrimSpace(action) == action && action != "" &&
		!strings.ContainsAny(action, "\r\n;&|")
}
