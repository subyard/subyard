package releasetransition

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/Subyard/Subyard/internal/domain"
)

const (
	maxIDLength       = 128
	maxRedactedText   = 256
	maxDiagnosticText = 512
)

var (
	ErrInvalid         = errors.New("invalid release transition record")
	ErrRegistryInvalid = errors.New("invalid release transition registry")
)

func invalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, arguments...))
}

func validateSafeID(value, field string) error {
	if len(value) > maxIDLength || !domain.SafeID(value) {
		return invalid("%s is not a safe ID", field)
	}
	return nil
}

func validateReleaseID(value ReleaseID, field string) error {
	return validateSafeID(string(value), field)
}

func validateTransactionID(value TransactionID) error {
	return validateSafeID(string(value), "transaction ID")
}

func validFingerprint(value Fingerprint) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func validateFingerprint(value Fingerprint, field string) error {
	if !validFingerprint(value) {
		return invalid("%s must be a lowercase SHA-256 fingerprint", field)
	}
	return nil
}

func validatePlanToken(value PlanToken) error {
	const prefix = "plan-v1-"
	if !strings.HasPrefix(string(value), prefix) ||
		!validFingerprint(Fingerprint(strings.TrimPrefix(string(value), prefix))) {
		return invalid("plan token is invalid")
	}
	return nil
}

func validateResumePlanToken(value PlanToken) error {
	const prefix = "resume-v1-"
	if !strings.HasPrefix(string(value), prefix) ||
		!validFingerprint(Fingerprint(strings.TrimPrefix(string(value), prefix))) {
		return invalid("resume plan token is invalid")
	}
	return nil
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

func releaseIDsEqual(left, right *ReleaseID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
