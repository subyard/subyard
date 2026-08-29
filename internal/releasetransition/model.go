package releasetransition

type StepObservation struct {
	Step        string      `json:"step"`
	Fingerprint Fingerprint `json:"fingerprint"`
}

type TransitionFacts struct {
	Goal                       Goal
	Releases                   ReleasePair
	Links                      ReleaseLinks
	Journal                    *JournalRecord
	Steps                      []StepObservation
	CurrentPlan                PlanToken
	CurrentIntents             []PlannerStepIntent
	VerifiedAuthorizationPlan  PlanToken
	CurrentArtifactDigest      Fingerprint
	CurrentRegistryDigest      Fingerprint
	CurrentCatalogDigest       Fingerprint
	CurrentAuthorizationDigest Fingerprint
	// FixedPointVerified means the domain ledger is current and all activation
	// reconcilers are verified against the target release at this observation.
	FixedPointVerified bool
}

func Evaluate(facts TransitionFacts) Outcome {
	if err := facts.Goal.Validate(); err != nil {
		return operatorOutcome(Outcome{}, CodeJournalInvalid, "release goal is invalid")
	}
	if err := facts.Releases.Validate(); err != nil || facts.Goal.Target != facts.Releases.Target {
		return operatorOutcome(Outcome{}, CodeJournalInvalid, "release pair is invalid")
	}
	if err := facts.Links.Validate(); err != nil {
		return operatorOutcome(Outcome{}, CodeActivationAmbiguous, "release links are ambiguous")
	}
	base := Outcome{
		Active: facts.Links.Active, Previous: cloneReleaseID(facts.Links.Previous),
		Target: facts.Goal.Target,
	}

	initial := linksEqual(facts.Links, ReleaseLinks{
		Active: facts.Releases.From, Previous: facts.Releases.Previous,
	})
	activated := linksEqual(facts.Links, ReleaseLinks{
		Active: facts.Releases.Target, Previous: releaseIDPointer(facts.Releases.From),
	})
	staged := facts.Releases.From != facts.Releases.Target && linksEqual(
		facts.Links,
		ReleaseLinks{Active: facts.Releases.From, Previous: releaseIDPointer(facts.Releases.Target)},
	)
	if facts.Journal == nil {
		switch {
		case facts.FixedPointVerified &&
			((facts.Releases.From == facts.Releases.Target && initial) || activated):
			return readyOutcome(base)
		case initial:
			base.Status = StatusMigrationRequired
			base.Code = CodeTransitionRequired
			base.Message = "the inspected release transition has not started"
			base.Retry = "run yard update"
			return base
		case activated:
			return operatorOutcome(base, CodeRecoveryAmbiguous, "the active target has no trusted completion evidence")
		default:
			return operatorOutcome(base, CodeActivationAmbiguous, "release links do not match a known transition checkpoint")
		}
	}

	journal := facts.Journal
	if err := journal.Validate(); err != nil || journal.Goal != facts.Goal ||
		!releasePairsEqual(journal.Releases, facts.Releases) {
		return operatorOutcome(base, CodeJournalInvalid, "release transition journal is invalid or foreign")
	}
	base.Transaction = transactionIDPointer(journal.Transaction)
	if !validCurrentBindings(facts) {
		return operatorOutcome(base, CodeJournalInvalid, "current release transition bindings are invalid")
	}
	if facts.VerifiedAuthorizationPlan != journal.AuthorizationPlan ||
		facts.CurrentPlan != journal.ResumePlan ||
		facts.CurrentArtifactDigest != journal.ArtifactDigest ||
		facts.CurrentRegistryDigest != journal.RegistryDigest ||
		facts.CurrentCatalogDigest != journal.CatalogDigest ||
		facts.CurrentAuthorizationDigest != journal.AuthorizationDigest {
		return operatorOutcome(base, CodePlanStale, "the authorized release transition plan is stale")
	}
	if err := validatePlannerIntents(facts.CurrentIntents); err != nil {
		return operatorOutcome(base, CodeJournalInvalid, "current planner step intents are invalid")
	}
	if !plannerIntentsMatchJournal(facts.CurrentIntents, journal.Steps) {
		return operatorOutcome(base, CodePlanStale, "current planner step intents do not match the authorized journal")
	}
	if !linksAllowed(
		journal.Checkpoint, initial, staged, activated,
		facts.Releases.From == facts.Releases.Target,
	) {
		return operatorOutcome(base, CodeActivationAmbiguous, "release links do not match the journaled activation checkpoint")
	}
	if code := validateObservedSteps(journal.Steps, facts.Steps); code != "" {
		message := "an observed migration resource does not match a journaled checkpoint"
		if code == CodeRecoveryAmbiguous {
			message = "observed migration resources are incomplete or foreign"
		}
		return operatorOutcome(base, code, message)
	}
	if journal.Checkpoint == JournalComplete && facts.FixedPointVerified {
		return readyOutcome(base)
	}
	base.Status = StatusRecovering
	base.Code = CodeRecoveryPending
	base.Message = "the authorized release transition can resume from observed facts"
	base.Retry = "run yard update"
	return base
}

func plannerIntentsMatchJournal(intents []PlannerStepIntent, steps []JournalStep) bool {
	if len(intents) != len(steps) {
		return false
	}
	for index, intent := range intents {
		step := steps[index]
		if intent.ID != step.ID || intent.Migration != step.Migration ||
			intent.Resource != step.Resource || intent.Decision != step.Decision ||
			intent.Expected != step.Expected || intent.Desired != step.Desired {
			return false
		}
	}
	return true
}

func validCurrentBindings(facts TransitionFacts) bool {
	return validateResumePlanToken(facts.CurrentPlan) == nil &&
		validatePlanToken(facts.VerifiedAuthorizationPlan) == nil &&
		validFingerprint(facts.CurrentArtifactDigest) &&
		validFingerprint(facts.CurrentRegistryDigest) &&
		validFingerprint(facts.CurrentCatalogDigest) &&
		validFingerprint(facts.CurrentAuthorizationDigest)
}

func validateObservedSteps(steps []JournalStep, observations []StepObservation) OutcomeCode {
	known := make(map[string]JournalStep, len(steps))
	for _, step := range steps {
		known[step.ID] = step
	}
	actual := make(map[string]Fingerprint, len(observations))
	for _, observation := range observations {
		if validateSafeID(observation.Step, "observed step ID") != nil ||
			!validFingerprint(observation.Fingerprint) {
			return CodeRecoveryAmbiguous
		}
		if _, exists := known[observation.Step]; !exists {
			return CodeRecoveryAmbiguous
		}
		if _, duplicate := actual[observation.Step]; duplicate {
			return CodeRecoveryAmbiguous
		}
		actual[observation.Step] = observation.Fingerprint
	}
	for _, step := range steps {
		if step.Checkpoint == StepVerified {
			continue
		}
		fingerprint, exists := actual[step.ID]
		if !exists {
			return CodeRecoveryAmbiguous
		}
		if fingerprint != step.Expected && fingerprint != step.Desired {
			return CodeMigrationStale
		}
	}
	return ""
}

func linksAllowed(checkpoint JournalCheckpoint, initial, staged, activated, sameRelease bool) bool {
	if sameRelease {
		return initial
	}
	switch checkpoint {
	case JournalAuthorized, JournalMigrating:
		return initial
	case JournalActivationIntent:
		return initial || staged || activated
	case JournalTargetActive, JournalReconciling, JournalComplete:
		return activated
	default:
		return false
	}
}

func readyOutcome(outcome Outcome) Outcome {
	outcome.Status = StatusReady
	outcome.ReachedGoal = true
	outcome.Code = CodeReady
	outcome.Message = "the target release is active and the transition is verified"
	outcome.Retry = ""
	return outcome
}

func operatorOutcome(outcome Outcome, code OutcomeCode, message string) Outcome {
	outcome.Status = StatusOperatorActionRequired
	outcome.Code = code
	outcome.Message = message
	outcome.Retry = "run yard update --check"
	return outcome
}

func linksEqual(left, right ReleaseLinks) bool {
	return left.Active == right.Active && releaseIDsEqual(left.Previous, right.Previous)
}

func releasePairsEqual(left, right ReleasePair) bool {
	return left.From == right.From && left.Target == right.Target &&
		releaseIDsEqual(left.Previous, right.Previous)
}

func cloneReleaseID(value *ReleaseID) *ReleaseID {
	if value == nil {
		return nil
	}
	return releaseIDPointer(*value)
}

func releaseIDPointer(value ReleaseID) *ReleaseID {
	copy := value
	return &copy
}

func transactionIDPointer(value TransactionID) *TransactionID {
	copy := value
	return &copy
}
