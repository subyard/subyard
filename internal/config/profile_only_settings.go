package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Profile-only settings are declared by an environment profile in its own
// profile.conf. Operators select profiles; they never assign these values.
var profileOnlySettings = []string{"YARD_CAPS", "YARD_DEVICES", "YARD_MOUNTS"}

// IsProfileOnlySetting reports whether only environment profiles declare name.
func IsProfileOnlySetting(name string) bool { return slices.Contains(profileOnlySettings, name) }

// IgnoredAssignment is a non-empty operator assignment of a profile-only
// setting. The loader discards it, as it discards retired settings; readiness
// reports it as configuration state with its repair. It is never a refusal.
type IgnoredAssignment struct {
	Yard, Name, Scope, Path string
	Line                    int
	Git                     bool
	// Nested marks a default performed inside another assignment's expression,
	// which config unset of the name cannot remove.
	Nested bool
	// authored reports that config unset at Scope writes exactly Path and
	// removes the assignment.
	authored bool
}

// Command reports whether the assignment came from the command environment.
func (assignment IgnoredAssignment) Command() bool { return assignment.Scope == "command" }

// Repair is the exact action that removes the assignment: the config unset
// command when it writes the file the value came from, otherwise the file
// line to remove. It is "" for the command environment.
func (assignment IgnoredAssignment) Repair() string {
	switch {
	case assignment.Command():
		return ""
	case assignment.authored:
		storage := "--local"
		if assignment.Git {
			storage = "--git"
		}
		return fmt.Sprintf("yard -Y %s config unset %s --scope %s %s", assignment.Yard, assignment.Name, assignment.Scope, storage)
	default:
		return fmt.Sprintf("remove line %d from %s", assignment.Line, assignment.Path)
	}
}

func (assignment IgnoredAssignment) String() string {
	if assignment.Command() {
		return assignment.Name + " in the command environment is ignored: only environment profiles declare it; unset it"
	}
	repair := "remove that line"
	if assignment.authored {
		repair = "remove it with " + assignment.Repair()
	}
	return fmt.Sprintf("%s in %s:%d is ignored: only environment profiles declare it; %s",
		assignment.Name, assignment.Path, assignment.Line, repair)
}

// IgnoredAssignments returns every ignored profile-only assignment in stable order.
func (loaded Loaded) IgnoredAssignments() []IgnoredAssignment {
	return slices.Clone(loaded.ignored)
}

// observeProfileOnly records a non-empty operator assignment of a
// profile-only setting; empty ones are tolerated.
func (tracker *settingTracker) observeProfileOnly(layer settingLayerID, name, value, path string, line int, nested bool) {
	if !IsProfileOnlySetting(name) || strings.TrimSpace(value) == "" {
		return
	}
	definition := tracker.layers[layer]
	tracker.ignored = append(tracker.ignored, IgnoredAssignment{
		Name: name, Scope: definition.Scope, Path: path, Line: line, Git: strings.HasPrefix(definition.Role, "Git "), Nested: nested,
	})
}

func (tracker *settingTracker) ignoredAssignments(yard, configHome string) []IgnoredAssignment {
	if yard == "" {
		yard = "default"
	}
	ignored := slices.Clone(tracker.ignored)
	for index := range ignored {
		ignored[index].Yard = yard
		ignored[index].authored = configHome != "" && !ignored[index].Nested &&
			ignored[index].Path == authoredSettingsPath(configHome, yard, ignored[index].Scope, ignored[index].Git)
	}
	slices.SortStableFunc(ignored, func(left, right IgnoredAssignment) int { return strings.Compare(left.String(), right.String()) })
	return slices.CompactFunc(ignored, func(left, right IgnoredAssignment) bool { return left.String() == right.String() })
}

// authoredSettingsPath is the scalar settings file that config set and unset
// write for scope, matching the loader's Git cache layout for --git.
func authoredSettingsPath(configHome, yard, scope string, git bool) string {
	root := configHome
	if git {
		root = filepath.Join(configHome, GitSettingsRelativePath)
	}
	switch SettingScope(scope) {
	case ScopeShared:
		return filepath.Join(root, "overrides", "shared", "config.env")
	case ScopeHost:
		return filepath.Join(root, "config.env")
	case ScopeYard:
		return filepath.Join(root, "yards", yard, "config.env")
	}
	return ""
}

// ConfigurationState is the structured form of configuration state that a
// readiness report carries beside its own result. It is never a refusal.
type ConfigurationState struct {
	State   string `json:"state"`
	Yard    string `json:"yard"`
	Setting string `json:"setting"`
	Scope   string `json:"scope"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Repair  string `json:"repair,omitempty"`
}

// ConfigurationState returns the structured form of the ignored assignment.
func (assignment IgnoredAssignment) ConfigurationState() ConfigurationState {
	return ConfigurationState{
		State: "ignored", Yard: assignment.Yard, Setting: assignment.Name, Scope: assignment.Scope,
		Path: assignment.Path, Line: assignment.Line, Repair: assignment.Repair(),
	}
}
