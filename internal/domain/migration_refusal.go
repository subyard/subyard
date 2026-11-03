package domain

import (
	"fmt"
	"slices"
	"strings"
)

// MigrationStateRefusal preserves an owner's explicit verdict and its public-safe
// reason. Dependency/read errors must not be wrapped as state verdicts.
func MigrationStateRefusal(yard string, cause error, reason string, names ...string) error {
	if cause == nil {
		return nil
	}
	safe := []string{}
	for _, name := range names {
		if len(name) <= 96 && SafeID(name) {
			safe = append(safe, name)
		} else {
			safe = append(safe, "unrecognized name")
		}
	}
	slices.Sort(safe)
	return &migrationStateRefusal{yard: yard, cause: cause, reason: reason, names: slices.Compact(safe)}
}

type migrationStateRefusal struct {
	yard, reason string
	cause        error
	names        []string
}

func (failure *migrationStateRefusal) Error() string  { return failure.cause.Error() }
func (failure *migrationStateRefusal) Unwrap() error  { return failure.cause }
func (*migrationStateRefusal) MigrationRefusal() bool { return true }
func (failure *migrationStateRefusal) ActivationDiagnostic() (string, string) {
	prefix := "registered yard: "
	if len(failure.yard) <= 63 && SafeName(failure.yard) {
		prefix = "yard " + failure.yard + ": "
	}
	reason := failure.reason
	if len(failure.names) != 0 {
		reason += ": " + strings.Join(failure.names, ", ")
	}
	if len(prefix+reason) > 512 {
		reason = fmt.Sprintf("%s (%d names; inspect the reported yard state)", failure.reason, len(failure.names))
	}
	return prefix + reason, "correct the reported yard state, then run yard update --check"
}

// MigrationRefusalDetails exposes only source-owned reason text and filtered names.
func (failure *migrationStateRefusal) MigrationRefusalDetails() (string, string, []string) {
	yard := failure.yard
	if len(yard) > 63 || !SafeName(yard) {
		yard = "unrecognized"
	}
	return yard, failure.reason, slices.Clone(failure.names)
}
