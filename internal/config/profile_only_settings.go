package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Profile-only settings are declared by an environment profile in its own
// profile.conf. Operators select profiles; they never assign these values.
//
// Ignoring and reporting an operator assignment of one (IgnoredAssignment and
// the ignored discard policy) is migration code for files written before
// these settings became profile-only. It can be removed once operator files
// no longer contain such assignments; config set already refuses new ones.
var profileOnlySettings = []string{"YARD_CAPS", "YARD_DEVICES", "YARD_MOUNTS"}

// IsProfileOnlySetting reports whether only environment profiles declare name.
func IsProfileOnlySetting(name string) bool { return slices.Contains(profileOnlySettings, name) }

// IgnoredAssignment is a non-empty operator assignment of a profile-only or
// retired setting, or an operator input this release does not know: an
// assignment of an unknown setting or an unknown profile or integration in a
// selection list. The loader discards all of them; readiness reports every one
// except retired ones, which installation inspection also reports, each with
// its repair. It is never a refusal.
type IgnoredAssignment struct {
	Yard, Name, Scope, Path string
	Line                    int
	Git                     bool
	Retired                 bool
	// Unknown is the kind of an input this release does not know. Item is
	// the unknown entry of a selection list; as a value, it is never rendered.
	Unknown UnknownInput
	Item    string
	// authored reports that config unset at Scope writes exactly Path and
	// removes the assignment.
	authored bool
}

// UnknownInput classifies an operator input that a newer release may define.
type UnknownInput string

const (
	UnknownSettingInput     UnknownInput = "unknown-setting"
	UnknownProfileInput     UnknownInput = "unknown-profile"
	UnknownIntegrationInput UnknownInput = "unknown-integration"
	// UnknownFileInput is a file under a Git source's overrides/agents that
	// no file setting of the loaded configuration maps; Name is its relative
	// path.
	UnknownFileInput UnknownInput = "unknown-file"
)

// Command reports whether the assignment came from the command environment.
func (assignment IgnoredAssignment) Command() bool { return assignment.Scope == "command" }

// Repair is the exact action that removes the assignment: the config unset
// command when it writes the file the value came from, otherwise the file
// line to remove. It is "" for the command environment. config unset refuses
// retired names, so their repair is always the file line.
func (assignment IgnoredAssignment) Repair() string {
	switch {
	case assignment.Unknown != "":
		// A newer release, or another host sharing Git settings, may use it.
		return ""
	case assignment.Command():
		return ""
	case assignment.authored && !assignment.Retired:
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
	source := "the command environment"
	if !assignment.Command() {
		source = fmt.Sprintf("%s:%d", assignment.Path, assignment.Line)
	}
	switch assignment.Unknown {
	case UnknownSettingInput:
		return fmt.Sprintf("%s in %s is not a setting of this release and is ignored; a newer release may use it", assignment.Name, source)
	case UnknownProfileInput:
		// A selected name is a value: it is never rendered.
		return fmt.Sprintf("%s in %s selects a profile this release does not ship; it is ignored", assignment.Name, source)
	case UnknownIntegrationInput:
		return fmt.Sprintf("%s in %s selects an integration this release does not ship; it is ignored", assignment.Name, source)
	case UnknownFileInput:
		return fmt.Sprintf("%s is no file setting of this configuration and is ignored; a newer release may use it", assignment.Path)
	}
	if assignment.Retired {
		return fmt.Sprintf("%s in %s:%d is retired and ignored; %s", assignment.Name, assignment.Path, assignment.Line, assignment.Repair())
	}
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
	return slices.DeleteFunc(slices.Clone(loaded.ignored), func(assignment IgnoredAssignment) bool { return assignment.Retired })
}

// RetiredAssignments returns every ignored retired file assignment in stable order.
func (loaded Loaded) RetiredAssignments() []IgnoredAssignment {
	return slices.DeleteFunc(slices.Clone(loaded.ignored), func(assignment IgnoredAssignment) bool { return !assignment.Retired })
}

// observeProfileOnly records a non-empty operator assignment of a
// profile-only or retired setting, and any operator assignment of an unknown
// setting; empty profile-only and retired ones are tolerated.
func (tracker *settingTracker) observeProfileOnly(layer settingLayerID, name, value, path string, line int) {
	retired := path != "" && (isRetiredSetting(name) || tracker.catalog.RetiredSetting(name))
	unknown := path != "" && !retired && tracker.catalog.UnknownSetting(name)
	if !unknown && (!retired && !IsProfileOnlySetting(name) || strings.TrimSpace(value) == "") {
		return
	}
	assignment := tracker.ignoredInput(layer, name, path, line)
	assignment.Retired = retired
	if unknown {
		assignment.Unknown = UnknownSettingInput
	}
	tracker.ignored = append(tracker.ignored, assignment)
}

// observeUnknownSelection records an unknown entry of a selection list, taken
// from the setting's effective assignment.
func (tracker *settingTracker) observeUnknownSelection(name string, kind UnknownInput, item string) {
	assignments := tracker.assignments[name]
	if len(assignments) == 0 {
		return
	}
	effective := assignments[len(assignments)-1]
	assignment := tracker.ignoredInput(effective.Layer, name, effective.Path, effective.Line)
	assignment.Unknown, assignment.Item = kind, item
	tracker.ignored = append(tracker.ignored, assignment)
}

// observeUnknownFiles records each regular file below a Git source's agents
// root that no file setting of this release maps, in lexical order. A read
// failure records nothing; the file settings themselves report it.
func (tracker *settingTracker) observeUnknownFiles(layer settingLayerID, agentsRoot string, mappings map[string]string) {
	known := map[string]bool{}
	for _, relative := range mappings {
		known[filepath.ToSlash(filepath.Clean(relative))] = true
	}
	_ = filepath.WalkDir(agentsRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(agentsRoot, path)
		if err != nil || known[filepath.ToSlash(relative)] {
			return nil
		}
		assignment := tracker.ignoredInput(layer, filepath.ToSlash(relative), path, 0)
		assignment.Unknown = UnknownFileInput
		tracker.ignored = append(tracker.ignored, assignment)
		return nil
	})
}

func (tracker *settingTracker) ignoredInput(layer settingLayerID, name, path string, line int) IgnoredAssignment {
	definition := tracker.layers[layer]
	if definition.Scope == "command" {
		path, line = "", 0
	}
	return IgnoredAssignment{Name: name, Scope: definition.Scope, Path: path, Line: line, Git: strings.HasPrefix(definition.Role, "Git ")}
}

// UnknownSetting reports an operator setting name this release does not
// define. A newer release may define it; the loader ignores such an
// assignment in operator scopes.
func (settings Catalog) UnknownSetting(name string) bool {
	return !IsProfileOnlySetting(name) && !settings.IsLegacySetting(name) && !knownSetting(settings, name)
}

func knownSetting(settings Catalog, name string) bool {
	_, ok := settings.LookupSetting(name)
	return ok
}

func (tracker *settingTracker) ignoredAssignments(yard, configHome string) []IgnoredAssignment {
	if yard == "" {
		yard = "default"
	}
	ignored := slices.Clone(tracker.ignored)
	for index := range ignored {
		assignment := &ignored[index]
		assignment.Yard = yard
		authored := authoredSettingsPath(configHome, yard, assignment.Scope, assignment.Git)
		assignment.authored = configHome != "" && assignment.Path == authored
		if !assignment.Git && assignment.Scope == "yard" {
			assignment.authored = assignment.authored || assignment.Path == filepath.Join(configHome, "yards", yard+".env")
		}
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
	Yard    string `json:"yard"`
	Setting string `json:"setting"`
	Scope   string `json:"scope"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Repair  string `json:"repair,omitempty"`
	// Reason is "profile-only" or an UnknownInput kind.
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// ConfigurationState returns the structured form of the ignored assignment.
func (assignment IgnoredAssignment) ConfigurationState() ConfigurationState {
	reason := string(assignment.Unknown)
	if reason == "" {
		reason = "profile-only"
	}
	return ConfigurationState{
		Yard: assignment.Yard, Setting: assignment.Name, Scope: assignment.Scope,
		Path: assignment.Path, Line: assignment.Line, Repair: assignment.Repair(),
		Reason: reason, Message: assignment.String(),
	}
}
