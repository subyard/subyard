package config

import (
	"path/filepath"
	"sort"
	"strings"
)

type SettingApplication string

const (
	SettingNextCommand SettingApplication = "next command"
	SettingYardInit    SettingApplication = "yard init"
	SettingConfigApply SettingApplication = "config apply"
)

type SettingResolution struct {
	Scope  string
	Role   string
	Path   string
	Line   int
	Value  string
	Status string
	Detail string
}

type SettingTrace struct {
	Name           string
	EffectiveValue string
	Kind           SettingKind
	Type           SettingValueType
	Aliases        []string
	Scopes         []SettingScope
	Syncable       bool
	Merge          string
	Application    SettingApplication
	Sensitive      bool
	Owner          string
	Resolutions    []SettingResolution
}

type ConfigurationLayer struct {
	Scope   string
	Role    string
	Path    string
	Present bool
}

type FileSettingMapping struct {
	Name        string
	Relative    string
	Application SettingApplication
}

type settingKind uint8

const (
	settingScalar settingKind = iota
	settingFile
	settingAny
)

type settingLayerID int

type settingLayer struct {
	ID      settingLayerID
	Scope   string
	Role    string
	Path    string
	Present bool
	Kind    settingKind
	Names   map[string]struct{}
}

type settingAssignment struct {
	Layer  settingLayerID
	Path   string
	Line   int
	Value  string
	Detail string
	Order  int
}

type settingTracker struct {
	integrations IntegrationSelection
	layers       []settingLayer
	assignments  map[string][]settingAssignment
	order        int
}

func newSettingTracker() *settingTracker {
	return &settingTracker{assignments: map[string][]settingAssignment{}}
}

func (tracker *settingTracker) addLayer(
	scope, role, path string,
	present bool,
	kind settingKind,
	names ...string,
) settingLayerID {
	id := settingLayerID(len(tracker.layers))
	applicableNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		applicableNames[name] = struct{}{}
	}
	tracker.layers = append(tracker.layers, settingLayer{
		ID: id, Scope: scope, Role: role, Path: path, Present: present, Kind: kind,
		Names: applicableNames,
	})
	return id
}

func (tracker *settingTracker) record(
	layer settingLayerID,
	name, value, path string,
	line int,
	detail string,
) {
	if _, ok := LookupSetting(name); !ok {
		return
	}
	tracker.order++
	tracker.assignments[name] = append(tracker.assignments[name], settingAssignment{
		Layer: layer, Path: path, Line: line, Value: value, Detail: detail, Order: tracker.order,
	})
}

func (tracker *settingTracker) normalize(name, value, detail string) {
	assignments := tracker.assignments[name]
	if len(assignments) == 0 {
		return
	}
	index := len(assignments) - 1
	assignments[index].Value = value
	if assignments[index].Detail == "" {
		assignments[index].Detail = detail
	} else {
		assignments[index].Detail += "; " + detail
	}
	tracker.assignments[name] = assignments
}

func (tracker *settingTracker) configurationLayers() []ConfigurationLayer {
	result := make([]ConfigurationLayer, 0, len(tracker.layers))
	for _, layer := range tracker.layers {
		if layer.Role == "command override" || layer.Role == "derived" {
			continue
		}
		result = append(result, ConfigurationLayer{
			Scope: layer.Scope, Role: layer.Role, Path: layer.Path, Present: layer.Present,
		})
	}
	return result
}

func (tracker *settingTracker) traces(values environment) map[string]SettingTrace {
	names := make(map[string]struct{}, len(catalog)+len(tracker.assignments))
	for name := range catalog {
		names[name] = struct{}{}
	}
	for name := range tracker.assignments {
		names[name] = struct{}{}
	}
	for name := range values {
		if _, ok := LookupSetting(name); ok {
			names[name] = struct{}{}
		}
	}

	result := make(map[string]SettingTrace, len(names))
	for name := range names {
		definition, ok := LookupSetting(name)
		if !ok {
			continue
		}
		assignments := tracker.assignments[name]
		effectiveOrder := 0
		for _, assignment := range assignments {
			if assignment.Order > effectiveOrder {
				effectiveOrder = assignment.Order
			}
		}
		trace := SettingTrace{
			Name: name, EffectiveValue: values[name],
			Kind: definition.Kind, Type: definition.Type,
			Aliases:  append([]string(nil), definition.Aliases...),
			Scopes:   append([]SettingScope(nil), definition.Scopes...),
			Syncable: definition.Syncable, Merge: definition.Merge,
			Application: definition.Application,
			Sensitive:   definition.Sensitive, Owner: definition.Owner,
		}
		for _, layer := range tracker.layers {
			if layer.Kind != settingAny && layer.Kind != internalSettingKind(definition.Kind) {
				continue
			}
			if len(layer.Names) != 0 {
				if _, ok := layer.Names[name]; !ok {
					continue
				}
			}
			found := false
			for _, assignment := range assignments {
				if assignment.Layer != layer.ID {
					continue
				}
				found = true
				status := "overridden"
				if assignment.Order == effectiveOrder {
					status = "effective"
				}
				path := assignment.Path
				if path == "" {
					path = layer.Path
				}
				trace.Resolutions = append(trace.Resolutions, SettingResolution{
					Scope: layer.Scope, Role: layer.Role, Path: path, Line: assignment.Line,
					Value: assignment.Value, Status: status, Detail: assignment.Detail,
				})
			}
			if !found {
				trace.Resolutions = append(trace.Resolutions, SettingResolution{
					Scope: layer.Scope, Role: layer.Role, Path: layer.Path, Status: "unset",
				})
			}
		}
		result[name] = trace
	}
	return result
}

func internalSettingKind(kind SettingKind) settingKind {
	if kind == SettingFile {
		return settingFile
	}
	return settingScalar
}

func SettingNames(settings map[string]SettingTrace) []string {
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func SyncableFileMappings(loaded Loaded) []FileSettingMapping {
	var result []FileSettingMapping
	seen := map[string]struct{}{}
	agentsRoot := filepath.Join(loaded.Context.Paths.ConfigDir, "agents")
	for name, trace := range loaded.Settings {
		if trace.Kind != SettingFile || !trace.Syncable {
			continue
		}
		for _, resolution := range trace.Resolutions {
			if resolution.Scope != "default" || resolution.Status == "unset" ||
				resolution.Value == "" {
				continue
			}
			relative, err := filepath.Rel(agentsRoot, filepath.Clean(resolution.Value))
			if err != nil || relative == "." || filepath.IsAbs(relative) ||
				relative == ".." ||
				strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				continue
			}
			relative = filepath.ToSlash(relative)
			if _, exists := seen[relative]; exists {
				continue
			}
			seen[relative] = struct{}{}
			result = append(result, FileSettingMapping{
				Name: name, Relative: relative, Application: trace.Application,
			})
			break
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Relative < result[right].Relative
	})
	return result
}

func ResolvedSettingCatalog(loaded Loaded) []SettingDefinition {
	result := SettingCatalog()
	for index := range result {
		result[index] = ResolvedSettingDefinition(loaded, result[index])
	}
	return result
}

func ResolvedSettingDefinition(loaded Loaded, definition SettingDefinition) SettingDefinition {
	trace, ok := loaded.Settings[definition.Name]
	if !ok {
		return definition
	}
	for _, resolution := range trace.Resolutions {
		if resolution.Scope == "default" && resolution.Status != "unset" {
			definition.Default = resolution.Value
			definition.HasDefault = true
		}
	}
	return definition
}
