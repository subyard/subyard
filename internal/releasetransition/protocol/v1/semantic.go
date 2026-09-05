package v1

// ValidateOutcome checks that an inspection and its public outcome describe
// one coherent schema v1 transition state for goal.
func (inspection Inspection) ValidateOutcome(goal Goal) error {
	if err := goal.validate(); err != nil {
		return err
	}
	if err := inspection.validateWire(); err != nil {
		return err
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
	if (outcome.Status == StatusReady || outcome.Status == StatusMigrationRequired) && len(inspection.Blockers) != 0 {
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

// ValidateInspection validates a standalone public outcome returned when a
// candidate cannot construct a safe inspection plan.
func (outcome Outcome) ValidateInspection(goal Goal) error {
	if err := goal.validate(); err != nil {
		return err
	}
	return outcome.validateInspectionOutcome(goal, false)
}

// ValidateConvergence checks a convergence response against the exact goal and
// prior inspection authorized by the updater.
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
	if inspection.Resume != nil && (outcome.Transaction == nil || *outcome.Transaction != *inspection.Resume) {
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
	} else if !stringsEqual(outcome.Previous, inspection.Outcome.Previous) {
		return invalid("ready convergence outcome changed the previous release unexpectedly")
	}
	if inspection.Resume != nil {
		return nil
	}
	if inspection.Assessment.Changed && outcome.Transaction == nil {
		return invalid("ready convergence outcome has no durable transaction")
	}
	if !inspection.Assessment.Changed && inspection.Outcome.Transaction != nil &&
		(outcome.Transaction == nil || *outcome.Transaction != *inspection.Outcome.Transaction) {
		return invalid("ready convergence outcome changed the inspected transaction")
	}
	return nil
}

func (goal Goal) validate() error {
	if err := validateSafeID(goal.Target, "target release"); err != nil {
		return err
	}
	if !validDirection(goal.Direction) {
		return invalid("unknown release direction %q", goal.Direction)
	}
	return nil
}

func (outcome Outcome) validateInspectionOutcome(goal Goal, allowUnknownActive bool) error {
	if err := outcome.validateWire(); err != nil {
		return err
	}
	if outcome.Target != goal.Target {
		return invalid("release inspection outcome targets a different release")
	}
	if outcome.Active == "" {
		if !allowUnknownActive || outcome.Status != StatusOperatorActionRequired ||
			outcome.Code != CodeRecoveryAmbiguous || outcome.Previous != nil || outcome.Transaction == nil {
			return invalid("release outcome has an unknown active release outside recovery ambiguity")
		}
	}
	if outcome.Previous != nil && *outcome.Previous == outcome.Active {
		return invalid("release inspection outcome has identical active and previous releases")
	}
	switch outcome.Status {
	case StatusReady:
		if !outcome.ReachedGoal || outcome.Active != goal.Target || outcome.Code != CodeReady || outcome.Retry != "" {
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
		if outcome.Code != CodeRecoveryPending && outcome.Code != CodeVerificationFailed &&
			outcome.Code != CodeDependencyUnavailable {
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

func convergenceLinksAllowed(outcome Outcome, goal Goal, inspection Inspection) bool {
	beforeActive, beforePrevious := inspection.Outcome.Active, inspection.Outcome.Previous
	afterActive, afterPrevious := goal.Target, (*string)(nil)
	if beforeActive == goal.Target {
		afterPrevious = beforePrevious
	} else {
		afterPrevious = stringPointer(beforeActive)
	}
	if sameLinks(outcome.Active, outcome.Previous, beforeActive, beforePrevious) ||
		sameLinks(outcome.Active, outcome.Previous, afterActive, afterPrevious) {
		return true
	}
	if beforeActive == goal.Target {
		return false
	}
	return sameLinks(outcome.Active, outcome.Previous, beforeActive, stringPointer(goal.Target))
}

func sameLinks(leftActive string, leftPrevious *string, rightActive string, rightPrevious *string) bool {
	return leftActive == rightActive && stringsEqual(leftPrevious, rightPrevious)
}

func stringsEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func stringPointer(value string) *string {
	return &value
}
