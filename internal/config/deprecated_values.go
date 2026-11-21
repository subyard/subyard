package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// DeprecatedAssignment is an operator assignment of a deprecated value alias,
// such as DEV_SUDO=0. The loader reads it as the canonical value, so it is
// never a refusal; readiness and validation report it with its replacement.
type DeprecatedAssignment struct {
	Yard, Name, Value, Replacement, Scope, Path string
	Line                                        int
	Git                                         bool
	// authored reports that config set at Scope writes exactly Path.
	authored bool
}

// Command reports whether the assignment came from the command environment.
func (assignment DeprecatedAssignment) Command() bool { return assignment.Scope == "command" }

// Repair is the exact action that replaces the alias: the config set command
// when it writes the file the value came from, otherwise the file line to edit.
// It is "" for the command environment.
func (assignment DeprecatedAssignment) Repair() string {
	switch {
	case assignment.Command():
		return ""
	case assignment.authored:
		storage := "--local"
		if assignment.Git {
			storage = "--git"
		}
		return fmt.Sprintf("yard -Y %s config set %s %s --scope %s %s", assignment.Yard, assignment.Name, assignment.Replacement, assignment.Scope, storage)
	default:
		return fmt.Sprintf("replace line %d of %s with %s=%s", assignment.Line, assignment.Path, assignment.Name, assignment.Replacement)
	}
}

func (assignment DeprecatedAssignment) String() string {
	read := fmt.Sprintf("%s=%s", assignment.Name, assignment.Value)
	if assignment.Command() {
		return fmt.Sprintf("%s in the command environment is deprecated and read as %s; use %s=%s",
			read, assignment.Replacement, assignment.Name, assignment.Replacement)
	}
	return fmt.Sprintf("%s in %s:%d is deprecated and read as %s; %s",
		read, assignment.Path, assignment.Line, assignment.Replacement, assignment.Repair())
}

// DeprecatedAssignments returns every deprecated value alias assignment in stable order.
func (loaded Loaded) DeprecatedAssignments() []DeprecatedAssignment {
	return slices.Clone(loaded.deprecated)
}

func (tracker *settingTracker) observeDeprecatedValue(layer settingLayerID, name, value, canonical, path string, line int) {
	definition := tracker.layers[layer]
	if definition.Role != "command override" && path == "" {
		return
	}
	assignment := DeprecatedAssignment{
		Name: name, Value: value, Replacement: canonical, Scope: definition.Scope,
		Path: path, Line: line, Git: strings.HasPrefix(definition.Role, "Git "),
	}
	if definition.Role == "command override" {
		assignment.Scope, assignment.Path, assignment.Line = "command", "", 0
	}
	tracker.deprecated = append(tracker.deprecated, assignment)
}

func (tracker *settingTracker) deprecatedAssignments(yard, configHome string) []DeprecatedAssignment {
	if yard == "" {
		yard = "default"
	}
	deprecated := slices.Clone(tracker.deprecated)
	for index := range deprecated {
		assignment := &deprecated[index]
		assignment.Yard = yard
		if assignment.Command() {
			continue
		}
		authored := authoredSettingsPath(configHome, yard, assignment.Scope, assignment.Git)
		assignment.authored = configHome != "" && authored != "" && assignment.Path == authored
		if !assignment.Git && assignment.Scope == "yard" {
			assignment.authored = assignment.authored || configHome != "" && assignment.Path == filepath.Join(configHome, "yards", yard+".env")
		}
	}
	slices.SortStableFunc(deprecated, func(left, right DeprecatedAssignment) int { return strings.Compare(left.String(), right.String()) })
	return slices.CompactFunc(deprecated, func(left, right DeprecatedAssignment) bool { return left.String() == right.String() })
}
