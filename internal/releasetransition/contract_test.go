package releasetransition

import (
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestPlanTokenBindsEveryMaterialInspectionFact(t *testing.T) {
	base := basePlanFacts()
	want, err := BindPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	again, err := BindPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	if want == "" || again != want {
		t.Fatalf("BindPlan(same facts) = %q then %q", want, again)
	}

	cases := map[string]func(*PlanFacts){
		"goal": func(facts *PlanFacts) {
			facts.Goal.Direction = DirectionActivatePrevious
		},
		"release pair": func(facts *PlanFacts) {
			facts.Releases.Previous = releasePtr("0.8.0")
		},
		"links": func(facts *PlanFacts) {
			facts.Links = activatedLinks(facts.Releases)
		},
		"artifact digest": func(facts *PlanFacts) {
			facts.ArtifactDigest = digestC
		},
		"registry digest": func(facts *PlanFacts) {
			facts.RegistryDigest = digestC
		},
		"catalog digest": func(facts *PlanFacts) {
			facts.CatalogDigest = digestC
		},
		"decision": func(facts *PlanFacts) {
			facts.Decisions[0].Decision = DecisionCanonicalize
		},
		"observation": func(facts *PlanFacts) {
			facts.Observations[0].Fingerprint = digestC
		},
		"assessment": func(facts *PlanFacts) {
			facts.Assessment.Recovery = domain.RecoveryIrreversible
		},
		"blocker": func(facts *PlanFacts) {
			facts.Blockers = []Blocker{{
				Code: CodePreconditionBlocked, Resource: "settings.power-mode",
				Message: "resource is busy", Retry: "run yard update --check",
			}}
		},
		"planner step intent": func(facts *PlanFacts) {
			facts.Intents[0].Desired = digestC
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := clonePlanFacts(base)
			change(&changed)
			got, err := BindPlan(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("BindPlan(changed %s) = unchanged token %q", name, got)
			}
		})
	}
}

func TestResumePlanBindsEveryImmutableRecoveryFact(t *testing.T) {
	authorizationPlan, err := BindPlan(basePlanFacts())
	if err != nil {
		t.Fatal(err)
	}
	base := baseResumePlanFacts(authorizationPlan, "tx-A")
	want, err := BindResumePlan(base)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*ResumePlanFacts){
		"goal":         func(facts *ResumePlanFacts) { facts.Goal.Direction = DirectionActivatePrevious },
		"release pair": func(facts *ResumePlanFacts) { facts.Releases.Previous = releasePtr("0.8.0") },
		"artifact":     func(facts *ResumePlanFacts) { facts.ArtifactDigest = digestC },
		"registry":     func(facts *ResumePlanFacts) { facts.RegistryDigest = digestC },
		"catalog":      func(facts *ResumePlanFacts) { facts.CatalogDigest = digestC },
		"observation scope": func(facts *ResumePlanFacts) {
			facts.ObservationScope = digestC
		},
		"assessment": func(facts *ResumePlanFacts) { facts.Assessment.Recovery = domain.RecoveryIrreversible },
		"decision":   func(facts *ResumePlanFacts) { facts.Decisions[0].Decision = DecisionCanonicalize },
		"intent":     func(facts *ResumePlanFacts) { facts.Intents[0].Desired = digestC },
		"blocker": func(facts *ResumePlanFacts) {
			facts.Blockers = []Blocker{{
				Code: CodePreconditionBlocked, Resource: "settings.power-mode",
				Message: "resource is busy", Retry: "run yard update --check",
			}}
		},
		"transaction": func(facts *ResumePlanFacts) { facts.Transaction = "tx-B" },
		"authorization plan": func(facts *ResumePlanFacts) {
			facts.AuthorizationPlan = PlanToken("plan-v1-" + strings.Repeat("c", 64))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := cloneResumePlanFacts(base)
			change(&changed)
			got, err := BindResumePlan(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("BindResumePlan(changed %s) = unchanged token %q", name, got)
			}
		})
	}
}

func TestResumePlanRecoveryIsInvariantAcrossExpectedDesiredAndActivatedActualFacts(t *testing.T) {
	authorizationPlan, err := BindPlan(basePlanFacts())
	if err != nil {
		t.Fatal(err)
	}
	transaction := TransactionID("tx-lifecycle")
	resumePlan, err := BindResumePlan(baseResumePlanFacts(authorizationPlan, transaction))
	if err != nil {
		t.Fatal(err)
	}
	if authorizationPlan == resumePlan {
		t.Fatalf("authorization and resume plan unexpectedly match: %q", resumePlan)
	}

	pair := releasePair()
	journal := validJournal(pair)
	journal.Transaction = transaction
	journal.AuthorizationPlan = authorizationPlan
	journal.ResumePlan = resumePlan
	journal.IntentDigest = bindJournalIntent(
		journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, journal.Steps,
	)

	casFacts := lifecycleTransitionFacts(
		journal, initialLinks(pair), JournalMigrating, StepIntent, digestB,
		resumePlan, authorizationPlan,
	)
	activatedFacts := lifecycleTransitionFacts(
		journal, activatedLinks(pair), JournalActivationIntent, StepVerified, digestB,
		resumePlan, authorizationPlan,
	)
	thirdFacts := lifecycleTransitionFacts(
		journal, initialLinks(pair), JournalMigrating, StepIntent, digestC,
		resumePlan, authorizationPlan,
	)
	for name, facts := range map[string]TransitionFacts{
		"after CAS": casFacts, "after activation": activatedFacts,
	} {
		t.Run(name, func(t *testing.T) {
			got := Evaluate(facts)
			if got.Status != StatusRecovering || got.Code != CodeRecoveryPending ||
				got.Transaction == nil || *got.Transaction != transaction {
				t.Fatalf("Evaluate(%s) = %#v", name, got)
			}
		})
	}
	got := Evaluate(thirdFacts)
	if got.Status != StatusOperatorActionRequired || got.Code != CodeMigrationStale {
		t.Fatalf("Evaluate(third actual) = %#v", got)
	}
}

func basePlanFacts() PlanFacts {
	pair := releasePair()
	return PlanFacts{
		Goal:             Goal{Target: pair.Target, Direction: DirectionActivateTarget},
		Releases:         pair,
		Links:            initialLinks(pair),
		ArtifactDigest:   digestA,
		RegistryDigest:   digestB,
		CatalogDigest:    digestD,
		ObservationScope: digestA,
		Assessment: domain.ActionAssessment{
			Action: "release.transition", Effect: domain.ActionMutation, Changed: true,
			Impacts:  []domain.ActionImpact{domain.ImpactYardRuntime},
			Recovery: domain.RecoveryReversible, Consequences: []string{"activate release 2.0.0"},
		},
		Decisions: []RedactedDecision{{
			Resource: "settings.power-mode", Scope: "yard", Decision: DecisionTransform,
			Result: "canonical-v2",
		}},
		Observations: []ResourceObservation{{
			Resource: "settings.power-mode", Class: "legacy-v1", Fingerprint: digestA,
		}},
		Intents: []PlannerStepIntent{{
			ID: "settings-v2", Migration: "settings-v2", Resource: "yard.hermes",
			Decision: DecisionTransform, Expected: digestA, Desired: digestB,
		}},
		Blockers: []Blocker{},
	}
}

func clonePlanFacts(facts PlanFacts) PlanFacts {
	facts.Releases.Previous = cloneReleasePtr(facts.Releases.Previous)
	facts.Links.Previous = cloneReleasePtr(facts.Links.Previous)
	facts.Decisions = append([]RedactedDecision(nil), facts.Decisions...)
	facts.Observations = append([]ResourceObservation(nil), facts.Observations...)
	facts.Intents = clonePlannerIntents(facts.Intents)
	facts.Blockers = append([]Blocker(nil), facts.Blockers...)
	facts.Assessment = facts.Assessment.Clone()
	return facts
}

func baseResumePlanFacts(authorizationPlan PlanToken, transaction TransactionID) ResumePlanFacts {
	facts := basePlanFacts()
	return ResumePlanFacts{
		Goal: facts.Goal, Releases: facts.Releases,
		ArtifactDigest: facts.ArtifactDigest, RegistryDigest: facts.RegistryDigest,
		CatalogDigest: facts.CatalogDigest, Assessment: facts.Assessment,
		ObservationScope: digestA,
		Decisions:        cloneDecisions(facts.Decisions), Intents: clonePlannerIntents(facts.Intents),
		Blockers: slicesCloneBlockers(facts.Blockers), Transaction: transaction,
		AuthorizationPlan: authorizationPlan,
	}
}

func cloneResumePlanFacts(facts ResumePlanFacts) ResumePlanFacts {
	facts.Releases.Previous = cloneReleasePtr(facts.Releases.Previous)
	facts.Assessment = facts.Assessment.Clone()
	facts.Decisions = cloneDecisions(facts.Decisions)
	facts.Intents = clonePlannerIntents(facts.Intents)
	facts.Blockers = slicesCloneBlockers(facts.Blockers)
	return facts
}

func cloneDecisions(values []RedactedDecision) []RedactedDecision {
	return append([]RedactedDecision(nil), values...)
}

func slicesCloneBlockers(values []Blocker) []Blocker {
	return append([]Blocker(nil), values...)
}

func lifecycleTransitionFacts(
	journal JournalRecord,
	links ReleaseLinks,
	journalCheckpoint JournalCheckpoint,
	stepCheckpoint StepCheckpoint,
	actual Fingerprint,
	currentPlan, verifiedAuthorization PlanToken,
) TransitionFacts {
	journal.Checkpoint = journalCheckpoint
	journal.Steps = append([]JournalStep(nil), journal.Steps...)
	journal.Steps[0].Checkpoint = stepCheckpoint
	if stepCheckpoint == StepVerified {
		evidence := validEvidence(journal.Releases, EvidenceVerified, digestB)
		evidence.Transaction = journal.Transaction
		journal.Steps[0].Evidence = &evidence
	} else {
		journal.Steps[0].Evidence = nil
	}
	return TransitionFacts{
		Goal: journal.Goal, Releases: journal.Releases, Links: links, Journal: &journal,
		Steps:       []StepObservation{{Step: journal.Steps[0].ID, Fingerprint: actual}},
		CurrentPlan: currentPlan, CurrentIntents: plannerIntentsFromJournal(journal.Steps),
		CurrentArtifactDigest:      journal.ArtifactDigest,
		CurrentRegistryDigest:      journal.RegistryDigest,
		CurrentCatalogDigest:       journal.CatalogDigest,
		CurrentAuthorizationDigest: journal.AuthorizationDigest,
		VerifiedAuthorizationPlan:  verifiedAuthorization,
	}
}

func clonePlannerIntents(intents []PlannerStepIntent) []PlannerStepIntent {
	return append([]PlannerStepIntent(nil), intents...)
}

func transactionPtr(value TransactionID) *TransactionID {
	copy := value
	return &copy
}
