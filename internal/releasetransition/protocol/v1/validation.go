package v1

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const (
	maxIDLength               = 128
	maxPlanItems              = 256
	maxRedactedText           = 256
	maxDiagnosticText         = 512
	maxAssessmentConsequences = 64
	maxOutcomeWarnings        = 64
)

var (
	ErrInvalid = errors.New("invalid release transition protocol v1 value")
	semverRE   = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
)

func invalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, arguments...))
}

func (request Request) validateWire() error {
	if request.SchemaVersion != SchemaVersion {
		return invalid("unsupported schema %d", request.SchemaVersion)
	}
	if request.Mode != ModeInspect && request.Mode != ModeConverge {
		return invalid("unknown process mode %q", request.Mode)
	}
	if !filepath.IsAbs(request.RuntimeRoot) || !filepath.IsAbs(request.ConfigHome) {
		return invalid("release transition roots must be absolute")
	}
	if request.Yard != "" && !safeName(request.Yard) {
		return invalid("yard is not a safe name")
	}
	if err := validateSafeID(request.Target, "target release"); err != nil {
		return err
	}
	if !validDirection(request.Direction) {
		return invalid("unknown release direction %q", request.Direction)
	}
	if err := validateFingerprint(request.ArtifactDigest, "artifact digest"); err != nil {
		return err
	}
	if request.RegistryDigest != "" {
		if err := validateFingerprint(request.RegistryDigest, "registry digest"); err != nil {
			return err
		}
	}
	if len(request.InheritedSettingIDs) > maxPlanItems {
		return invalid("too many inherited setting IDs")
	}
	for _, setting := range request.InheritedSettingIDs {
		if err := validateSafeID(setting, "inherited setting ID"); err != nil {
			return err
		}
	}
	if request.SourceIngress != nil {
		if err := request.SourceIngress.Validate(); err != nil {
			return err
		}
	}
	if request.Replacement != nil {
		if err := request.Replacement.Validate(); err != nil {
			return err
		}
	}
	switch request.Mode {
	case ModeInspect:
		if request.Execution != nil {
			return invalid("inspect request contains an execution")
		}
	case ModeConverge:
		if request.Execution == nil {
			return invalid("converge request has no execution")
		}
	}
	return nil
}

func (response Response) validateWire() error {
	if response.SchemaVersion != SchemaVersion {
		return invalid("unsupported schema %d", response.SchemaVersion)
	}
	if (response.Inspection == nil) == (response.Outcome == nil) {
		return invalid("response must contain exactly one inspection or outcome")
	}
	if response.Inspection != nil {
		if err := response.Inspection.validateWire(); err != nil {
			return err
		}
	}
	if response.Outcome != nil {
		if err := response.Outcome.validateWire(); err != nil {
			return err
		}
	}
	return nil
}

func (request SourceIngressRequest) Validate() error {
	if request.SchemaVersion != SourceIngressSchemaVersion || request.Kind != SourceIngressPreGo {
		return invalid("unknown source ingress descriptor schema or kind")
	}
	roles := []struct {
		name string
		path string
	}{
		{"source root", request.SourceRoot},
		{"data home", request.DataHome},
		{"launcher directory", request.BinDir},
		{"interactive shell rc", request.RC},
		{"login shell rc", request.LoginRC},
	}
	for _, role := range roles {
		if err := validateSourceIngressRolePath(role.path); err != nil {
			return invalid("%s is invalid: %v", role.name, err)
		}
	}
	return nil
}

func (replacement JournalReplacement) Validate() error {
	if err := validateSafeID(replacement.Transaction, "transaction ID"); err != nil {
		return err
	}
	if err := validateFingerprint(replacement.Fingerprint, "replacement journal fingerprint"); err != nil {
		return err
	}
	switch replacement.Reason {
	case JournalReplacementPreActivationPlanStale:
		if replacement.SourceVersion != "" {
			return invalid("pre-activation journal replacement has a source version")
		}
	case JournalReplacementPostActivationScopeV0111:
		if !validCanonicalSemver(replacement.SourceVersion) {
			return invalid("post-activation journal replacement has an invalid source version")
		}
	default:
		return invalid("unknown journal replacement reason %q", replacement.Reason)
	}
	return nil
}

func (inspection Inspection) validateWire() error {
	if inspection.Resume == nil {
		if !validPlanToken(inspection.Plan, "plan-v1-") {
			return invalid("plan token is invalid")
		}
	} else {
		if err := validateSafeID(*inspection.Resume, "transaction ID"); err != nil {
			return err
		}
		if !validPlanToken(inspection.Plan, "resume-v1-") {
			return invalid("resume plan token is invalid")
		}
	}
	if err := validateV2Assessment(inspection.Assessment); err != nil {
		return err
	}
	if len(inspection.Decisions) > maxPlanItems || len(inspection.Blockers) > maxPlanItems {
		return invalid("inspection contains too many public items")
	}
	for _, decision := range inspection.Decisions {
		if err := decision.validate(); err != nil {
			return err
		}
	}
	for _, blocker := range inspection.Blockers {
		if err := blocker.validate(); err != nil {
			return err
		}
	}
	if inspection.Outcome == nil {
		return invalid("release inspection has no public outcome")
	}
	return inspection.Outcome.validateWire()
}

func (outcome Outcome) validateWire() error {
	if !validStatus(outcome.Status) {
		return invalid("release outcome has unknown status %q", outcome.Status)
	}
	if outcome.Active != "" {
		if err := validateSafeID(outcome.Active, "active release"); err != nil {
			return err
		}
	}
	if outcome.Previous != nil {
		if err := validateSafeID(*outcome.Previous, "previous release"); err != nil {
			return err
		}
	}
	if err := validateSafeID(outcome.Target, "target release"); err != nil {
		return err
	}
	if !validOutcomeCode(outcome.Code) {
		return invalid("release outcome has unknown code %q", outcome.Code)
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
		if err := validateSafeID(*outcome.Transaction, "transaction ID"); err != nil {
			return err
		}
	}
	if len(outcome.Warnings) > maxOutcomeWarnings {
		return invalid("release outcome has too many warnings")
	}
	for _, warning := range outcome.Warnings {
		if err := validateText(warning, "release outcome warning", maxDiagnosticText, true); err != nil {
			return err
		}
	}
	if !slices.IsSorted(outcome.Warnings) {
		return invalid("release outcome warnings are not canonical")
	}
	return nil
}

func validateV2Assessment(assessment ActionAssessment) error {
	if err := validateSafeID(assessment.Action, "action assessment ID"); err != nil {
		return err
	}
	if assessment.Action != "release.transition.v2" || assessment.Effect != ActionEffectMutation ||
		assessment.Recovery != RecoveryReversible {
		return invalid("assessment metadata does not match action %q", "release.transition.v2")
	}
	wantImpacts := []string{ActionImpactLocalMetadata, ActionImpactPersistentData, ActionImpactYardRuntime}
	if !slices.Equal(assessment.Impacts, wantImpacts) {
		return invalid("assessment impacts do not match action %q", "release.transition.v2")
	}
	if len(assessment.Consequences) > maxAssessmentConsequences {
		return invalid("action assessment has too many consequences")
	}
	for _, consequence := range assessment.Consequences {
		if err := validateText(consequence, "action assessment consequence", maxDiagnosticText, true); err != nil {
			return err
		}
	}
	if assessment.Changed && len(assessment.Consequences) == 0 {
		return invalid("changed action %q requires a consequence", assessment.Action)
	}
	return nil
}

func (decision RedactedDecision) validate() error {
	if err := validateSafeID(decision.Resource, "decision resource"); err != nil {
		return err
	}
	if decision.Scope != "" {
		if err := validateSafeID(decision.Scope, "decision scope"); err != nil {
			return err
		}
	}
	if !validDecision(decision.Decision) {
		return invalid("unknown decision %q", decision.Decision)
	}
	return validateText(decision.Result, "redacted decision result", maxRedactedText, false)
}

func (blocker Blocker) validate() error {
	if !validOutcomeCode(blocker.Code) || blocker.Code == CodeReady {
		return invalid("unknown blocker code %q", blocker.Code)
	}
	if blocker.Resource != "" {
		if err := validateSafeID(blocker.Resource, "blocked resource"); err != nil {
			return err
		}
	}
	if err := validateText(blocker.Message, "blocker message", maxDiagnosticText, true); err != nil {
		return err
	}
	return validateSafeAction(blocker.Retry)
}

func validDirection(value string) bool {
	return value == DirectionActivateTarget || value == DirectionActivatePrevious
}

func validStatus(value string) bool {
	return value == StatusReady || value == StatusMigrationRequired || value == StatusRecovering ||
		value == StatusOperatorActionRequired
}

func validOutcomeCode(value string) bool {
	switch value {
	case CodeReady, CodeTransitionRequired, CodeRecoveryPending, CodeRegistryInvalid,
		CodeUnsupportedEpoch, CodeUnsupportedKind, CodeConfirmationRequired,
		CodeOperationDeclined, CodePlanStale, CodeMigrationStale, CodeResourceConflict,
		CodePreconditionBlocked, CodeDependencyUnavailable, CodeVerificationFailed,
		CodeActivationAmbiguous, CodeRecoveryAmbiguous, CodeJournalInvalid,
		CodeRollbackIncompatible, CodeRollbackExpired:
		return true
	default:
		return false
	}
}

func validDecision(value string) bool {
	switch value {
	case DecisionPreserve, DecisionTransform, DecisionCanonicalize, DecisionReset,
		DecisionQuarantine, DecisionBlock:
		return true
	default:
		return false
	}
}

func validateSafeID(value, field string) error {
	if len(value) > maxIDLength || !safeID(value) {
		return invalid("%s is not a safe ID", field)
	}
	return nil
}

func safeID(value string) bool {
	if value == "" || value == "." || value == ".." || strings.HasPrefix(value, "-") {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && char != '.' && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func safeName(value string) bool {
	if value == "" || !((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= '0' && value[0] <= '9')) {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func validateFingerprint(value, field string) error {
	if len(value) != 64 || strings.IndexFunc(value, func(char rune) bool {
		return !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f'))
	}) >= 0 {
		return invalid("%s must be a lowercase SHA-256 fingerprint", field)
	}
	return nil
}

func validPlanToken(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	return validateFingerprint(strings.TrimPrefix(value, prefix), "plan fingerprint") == nil
}

func validateText(value, field string, maximum int, required bool) error {
	if strings.TrimSpace(value) != value || (required && value == "") || len(value) > maximum ||
		strings.ContainsFunc(value, unicode.IsControl) {
		return invalid("%s is unsafe or unbounded", field)
	}
	return nil
}

func validateSafeAction(value string) error {
	if err := validateText(value, "retry action", maxDiagnosticText, true); err != nil {
		return err
	}
	if strings.ContainsAny(value, ";&|") {
		return invalid("retry action must contain exactly one safe action")
	}
	return nil
}

func validateSourceIngressRolePath(path string) error {
	const maximumSourceIngressPath = 4096
	if path == "" || len(path) > maximumSourceIngressPath || !filepath.IsAbs(path) || path == string(filepath.Separator) ||
		filepath.Clean(path) != path || strings.ContainsFunc(path, unicode.IsControl) {
		return errors.New("path must be clean, absolute, non-root, and bounded")
	}
	return nil
}

func validCanonicalSemver(value string) bool {
	matches := semverRE.FindStringSubmatch(value)
	if matches == nil {
		return false
	}
	for _, component := range matches[1:4] {
		if _, err := strconv.ParseUint(component, 10, 64); err != nil {
			return false
		}
	}
	if matches[4] != "" {
		for _, identifier := range strings.Split(matches[4], ".") {
			if len(identifier) > 1 && identifier[0] == '0' && identifier[0] >= '0' && identifier[0] <= '9' &&
				strings.IndexFunc(identifier, func(char rune) bool { return char < '0' || char > '9' }) == -1 {
				return false
			}
			if strings.IndexFunc(identifier, func(char rune) bool { return char < '0' || char > '9' }) == -1 {
				if _, err := strconv.ParseUint(identifier, 10, 64); err != nil {
					return false
				}
			}
		}
	}
	return true
}
