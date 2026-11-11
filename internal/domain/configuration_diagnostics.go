package domain

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// ValidRuntimeDiagnosticValue keeps profile and activation diagnostics within
// the same closed public vocabulary.
func ValidRuntimeDiagnosticValue(value string) bool {
	return slices.Contains([]string{"unknown", "absent", "present", "active", "inactive", "enabled", "disabled", "not-found", "failed", "drift", "current", "changed", "matched", "mismatch", "ready", "not-ready", "missing"}, value)
}

// ConfigurationDifference describes an independently observed setting difference.
// Callers must only supply values from a catalog that identifies sensitive inputs.
type ConfigurationDifferenceKind uint8

const (
	ConfigurationValues ConfigurationDifferenceKind = iota
	ConfigurationSensitive
	ConfigurationContentChanged
	ConfigurationContentUnknown
	ConfigurationStateUnknown
	ConfigurationMaterializedChanged
)

type ConfigurationDifference struct {
	Name, Installed, Desired string
	InstalledSet, DesiredSet bool
	Kind                     ConfigurationDifferenceKind
}

// Value formats a typed difference without exposing sensitive inputs.
func (diff ConfigurationDifference) Value() string {
	switch diff.Kind {
	case ConfigurationSensitive:
		return "changed"
	case ConfigurationContentChanged:
		return "content changed"
	case ConfigurationContentUnknown:
		return "installed or desired content unknown"
	case ConfigurationStateUnknown:
		return "installed state unknown"
	case ConfigurationMaterializedChanged:
		return "materialized content differs (installed source unknown)"
	case ConfigurationValues:
		return diagnosticValue(diff.Installed, diff.InstalledSet) + " -> " + diagnosticValue(diff.Desired, diff.DesiredSet)
	default:
		return "installed state unknown"
	}
}

// YardConfigurationDiagnostic is an observed yard's configuration, or an explicit
// explanation that its installed state could not be observed.
type YardConfigurationDiagnostic struct {
	Yard, Unavailable string
	Differences       []ConfigurationDifference
}

func diagnosticValue(value string, set bool) string {
	if !set {
		return "(unset)"
	}
	if value == "" {
		return "(empty)"
	}
	if strings.ContainsFunc(value, unicode.IsControl) {
		return strconv.Quote(value)
	}
	return value
}

// FormatConfigurationDiagnostics returns stable, complete human-readable rows.
// Sensitive values are never rendered, even when only one side is present.
func FormatConfigurationDiagnostics(yards []YardConfigurationDiagnostic) string {
	yards = slices.Clone(yards)
	slices.SortFunc(yards, func(a, b YardConfigurationDiagnostic) int { return strings.Compare(a.Yard, b.Yard) })
	rows := make([]string, 0, len(yards))
	for _, yard := range yards {
		row := "yard " + yard.Yard + ":"
		if yard.Unavailable != "" {
			row += " " + yard.Unavailable
		}
		diffs := slices.Clone(yard.Differences)
		slices.SortFunc(diffs, func(a, b ConfigurationDifference) int { return strings.Compare(a.Name, b.Name) })
		for _, diff := range diffs {
			row += "\n  " + diff.Name + "  " + diff.Value()
		}
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}
