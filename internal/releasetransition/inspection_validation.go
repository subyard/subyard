package releasetransition

import (
	"slices"
)

// ValidateOutcome is the single semantic boundary for a public inspection.
// It rejects structurally valid responses whose plan, blockers, recovery
// identity, or public outcome describe different transition states.
func (inspection Inspection) ValidateOutcome(goal Goal) error {
	if err := goal.Validate(); err != nil {
		return err
	}
	if inspection.Resume == nil {
		if err := validatePlanToken(inspection.Plan); err != nil {
			return err
		}
	} else {
		if err := validateTransactionID(*inspection.Resume); err != nil {
			return err
		}
		if err := validateResumePlanToken(inspection.Plan); err != nil {
			return err
		}
	}
	if err := validateV2InspectionAssessment(inspection); err != nil {
		return err
	}
	if len(inspection.Decisions) > MaxPlanItems || len(inspection.Blockers) > MaxPlanItems {
		return invalid("inspection contains too many public items")
	}
	for _, decision := range inspection.Decisions {
		if err := decision.Validate(); err != nil {
			return err
		}
	}
	for _, blocker := range inspection.Blockers {
		if err := blocker.Validate(); err != nil {
			return err
		}
	}
	if inspection.Outcome == nil {
		return invalid("release inspection has no public outcome")
	}
	if err := inspection.Outcome.validateInspectionOutcome(goal, false); err != nil {
		return err
	}

	outcome := inspection.Outcome
	if len(inspection.Blockers) != 0 {
		blocker := inspection.Blockers[0]
		if outcome.Status != StatusOperatorActionRequired || outcome.Code != blocker.Code ||
			outcome.Message != blocker.Message || outcome.Retry != blocker.Retry {
			return invalid("release inspection blockers and public outcome disagree")
		}
	}
	if inspection.Resume != nil {
		if outcome.Status != StatusRecovering && outcome.Status != StatusOperatorActionRequired {
			return invalid("release recovery identity has an incompatible public status")
		}
		if outcome.Transaction == nil || *outcome.Transaction != *inspection.Resume {
			return invalid("release recovery transaction and public outcome disagree")
		}
	}
	if (outcome.Status == StatusReady || outcome.Status == StatusMigrationRequired) &&
		len(inspection.Blockers) != 0 {
		return invalid("non-blocked release status contains operator blockers")
	}
	if outcome.Status == StatusReady && inspection.Assessment.Changed {
		return invalid("ready release inspection still assesses a change")
	}
	if (outcome.Status == StatusMigrationRequired || outcome.Status == StatusRecovering) &&
		!inspection.Assessment.Changed {
		return invalid("pending release inspection assesses no change")
	}
	return nil
}

// ValidateInspection validates a standalone public outcome returned instead
// of an inspection when the candidate cannot construct a safe plan.
func (outcome Outcome) ValidateInspection(goal Goal) error {
	return outcome.validateInspectionOutcome(goal, false)
}

// ValidateConvergence is the semantic boundary for a public convergence
// response. Non-ready outcomes retain their ordinary public meaning; a ready
// outcome must also prove that it reached the exact inspected release identity.
func (outcome Outcome) ValidateConvergence(goal Goal, inspection Inspection) error {
	if err := inspection.ValidateOutcome(goal); err != nil {
		return err
	}
	if err := outcome.validateInspectionOutcome(goal, true); err != nil {
		return err
	}
	if outcome.Active != "" && !convergenceLinksAllowed(outcome, goal, inspection) {
		return invalid("convergence outcome reported impossible release links")
	}
	if inspection.Resume != nil &&
		(outcome.Transaction == nil || *outcome.Transaction != *inspection.Resume) {
		return invalid("convergence outcome changed the inspected recovery transaction")
	}
	if outcome.Status != StatusReady {
		return nil
	}
	if outcome.Active != goal.Target {
		return invalid("ready convergence outcome did not activate the target release")
	}
	if inspection.Outcome.Active != goal.Target {
		if outcome.Previous == nil || *outcome.Previous != inspection.Outcome.Active {
			return invalid("ready convergence outcome did not retain the inspected active release")
		}
	} else if !releaseIDsEqual(outcome.Previous, inspection.Outcome.Previous) {
		return invalid("ready convergence outcome changed the previous release unexpectedly")
	}

	if inspection.Resume != nil {
		// Exact recovery identity was checked for every status above.
	} else if inspection.Assessment.Changed && outcome.Transaction == nil {
		return invalid("ready convergence outcome has no durable transaction")
	} else if !inspection.Assessment.Changed && inspection.Outcome.Transaction != nil &&
		(outcome.Transaction == nil || *outcome.Transaction != *inspection.Outcome.Transaction) {
		return invalid("ready convergence outcome changed the inspected transaction")
	}
	return nil
}

func convergenceLinksAllowed(outcome Outcome, goal Goal, inspection Inspection) bool {
	before := ReleaseLinks{
		Active:   inspection.Outcome.Active,
		Previous: cloneReleaseID(inspection.Outcome.Previous),
	}
	after := ReleaseLinks{Active: goal.Target}
	if before.Active == goal.Target {
		after.Previous = cloneReleaseID(before.Previous)
	} else {
		after.Previous = releaseIDPointer(before.Active)
	}
	reported := ReleaseLinks{Active: outcome.Active, Previous: cloneReleaseID(outcome.Previous)}
	if releaseLinksEqual(reported, before) || releaseLinksEqual(reported, after) {
		return true
	}
	if before.Active == goal.Target {
		return false
	}
	staged := ReleaseLinks{Active: before.Active, Previous: releaseIDPointer(goal.Target)}
	return releaseLinksEqual(reported, staged)
}

func releaseLinksEqual(left ReleaseLinks, right ReleaseLinks) bool {
	return left.Active == right.Active && releaseIDsEqual(left.Previous, right.Previous)
}

func validateV2InspectionAssessment(inspection Inspection) error {
	assessment := inspection.Assessment
	if err := validateAssessmentShape(assessment); err != nil {
		return err
	}
	policy, err := newV2ActionPolicy()
	if err != nil {
		return err
	}
	if _, _, err := policy.Resolve(assessment); err != nil {
		return err
	}
	return nil
}

func (outcome Outcome) validateInspectionOutcome(goal Goal, allowUnknownActive bool) error {
	if outcome.Target != goal.Target {
		return invalid("release inspection outcome targets a different release")
	}
	if outcome.Active == "" {
		if !allowUnknownActive || outcome.Status != StatusOperatorActionRequired ||
			outcome.Code != CodeRecoveryAmbiguous || outcome.Previous != nil ||
			outcome.Transaction == nil {
			return invalid("release outcome has an unknown active release outside recovery ambiguity")
		}
	} else {
		if err := validateReleaseID(outcome.Active, "active release"); err != nil {
			return err
		}
	}
	if outcome.Previous != nil {
		if err := validateReleaseID(*outcome.Previous, "previous release"); err != nil {
			return err
		}
		if *outcome.Previous == outcome.Active {
			return invalid("release inspection outcome has identical active and previous releases")
		}
	}
	if !validOutcomeCode(outcome.Code) {
		return invalid("release inspection outcome has unknown code %q", outcome.Code)
	}
	if err := validateText(outcome.Message, "release outcome message", maxDiagnosticText, true); err != nil {
		return err
	}
	if outcome.Retry != "" {
		if err := validateSafeAction(outcome.Retry); err != nil {
			return err
		}
	}
	if outcome.Transaction != nil {
		if err := validateTransactionID(*outcome.Transaction); err != nil {
			return err
		}
	}
	if len(outcome.Warnings) > 64 {
		return invalid("release inspection outcome has too many warnings")
	}
	for _, warning := range outcome.Warnings {
		if err := validateText(warning, "release outcome warning", maxDiagnosticText, true); err != nil {
			return err
		}
	}
	if !slices.IsSorted(outcome.Warnings) {
		return invalid("release inspection warnings are not canonical")
	}

	switch outcome.Status {
	case StatusReady:
		if !outcome.ReachedGoal || outcome.Active != goal.Target ||
			outcome.Code != CodeReady || outcome.Retry != "" {
			return invalid("ready release inspection outcome is inconsistent")
		}
	case StatusMigrationRequired:
		if outcome.ReachedGoal || outcome.Code != CodeTransitionRequired || outcome.Retry == "" ||
			outcome.Transaction != nil {
			return invalid("migration-required release inspection outcome is inconsistent")
		}
	case StatusRecovering:
		if outcome.ReachedGoal || outcome.Retry == "" || outcome.Transaction == nil {
			return invalid("recovering release inspection outcome is inconsistent")
		}
		switch outcome.Code {
		case CodeRecoveryPending, CodeVerificationFailed, CodeDependencyUnavailable:
		default:
			return invalid("recovering release inspection outcome is inconsistent")
		}
	case StatusOperatorActionRequired:
		if outcome.ReachedGoal || outcome.Code == CodeReady || outcome.Retry == "" {
			return invalid("operator-action release inspection outcome is inconsistent")
		}
	default:
		return invalid("release inspection outcome has unknown status %q", outcome.Status)
	}
	return nil
}
