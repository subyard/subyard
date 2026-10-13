package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ComponentSelection is a projection of the same loaded resolver snapshot used
// by apply. Runtime health is deliberately supplied by the observing boundary.
type ComponentSelection struct {
	ID           string              `json:"id"`
	Kind         string              `json:"kind"`
	Requested    bool                `json:"requested"`
	Effective    bool                `json:"effective"`
	Eligible     bool                `json:"eligible"`
	Present      bool                `json:"selection_present"`
	Reason       string              `json:"reason"`
	SourceGroup  string              `json:"source_group"`
	Source       SettingResolution   `json:"source"`
	Overridden   []SettingResolution `json:"overridden"`
	Dependencies []string            `json:"dependency_reasons"`
}

func ComponentSelectionSource(trace SettingTrace) (SettingResolution, []SettingResolution, string) {
	source := SettingResolution{Status: "unset"}
	overridden := []SettingResolution{}
	for _, resolution := range trace.Resolutions {
		if resolution.Status == "effective" {
			source = resolution
		} else {
			overridden = append(overridden, resolution)
		}
	}
	group := "shipped defaults"
	switch {
	case source.Scope == "command":
		group = "command override"
	case strings.Contains(source.Role, "Git"):
		group = "Git settings"
	case source.Role == "scalar settings" || source.Role == "file settings":
		group = "local overrides"
	}
	return source, overridden, group
}

func (loaded Loaded) IntegrationInventory() []ComponentSelection {
	names := map[string]bool{} // Values track descriptor retirement.
	for key := range loaded.Environment {
		if !strings.HasPrefix(key, "AGENT_") {
			continue
		}
		for _, suffix := range integrationMetadataSuffixes {
			if strings.HasSuffix(key, "_"+suffix) {
				names[strings.TrimSuffix(strings.TrimPrefix(key, "AGENT_"), "_"+suffix)] = false
				break
			}
		}
	}
	for _, declaration := range loaded.Catalog.Integrations() {
		names[declaration.Name] = declaration.Retired
	}
	source, overridden, group := ComponentSelectionSource(loaded.Settings["CODING_TOOL_INTEGRATIONS"])
	result := make([]ComponentSelection, 0, len(names))
	for name, retired := range names {
		entry := ComponentSelection{ID: name, Kind: "coding-integration", Requested: slices.Contains(loaded.Integrations.Requested, name),
			Effective: slices.Contains(loaded.Integrations.Effective, name), Eligible: loaded.Integrations.AllowsCodingTools && !retired,
			Present: loaded.Integrations.Present, Source: source, SourceGroup: group, Overridden: overridden,
			Dependencies: slices.Clone(loaded.Integrations.DependencyReasons[name])}
		switch {
		case retired:
			entry.Reason = "retired"
		case !entry.Eligible:
			entry.Reason = "role-restricted"
		case !entry.Requested && entry.Effective:
			entry.Reason = "dependency"
		case entry.Requested && group == "shipped defaults":
			entry.Reason = "inherited-default"
		case entry.Requested && group == "Git settings":
			entry.Reason = "Git-fallback"
		case entry.Requested:
			entry.Reason = "explicit-selection"
		case !entry.Present:
			entry.Reason = "selection-absent"
		default:
			entry.Reason = "excluded-by-replace-list"
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (loaded Loaded) IntegrationSelectionExplanation() string {
	source, _, group := ComponentSelectionSource(loaded.Settings["CODING_TOOL_INTEGRATIONS"])
	return fmt.Sprintf("yard %s: requested coding integrations [%s], effective [%s], from %s (%s scope, %s:%d)",
		loaded.Context.YardName, strings.Join(loaded.Integrations.Requested, " "), strings.Join(loaded.Integrations.Effective, " "), group, source.Scope, source.Path, source.Line)
}

func (loaded Loaded) EnvironmentProfileSelectionExplanation() string {
	value, present := loaded.Environment["ENVIRONMENT_PROFILES"]
	source, _, group := ComponentSelectionSource(loaded.Settings["ENVIRONMENT_PROFILES"])
	if !present {
		return fmt.Sprintf("yard %s: environment profiles use implicit shipped provision defaults", loaded.Context.YardName)
	}
	if value == "" {
		return fmt.Sprintf("yard %s: requested environment profiles [], from %s (%s scope, %s:%d); eligible descriptor-less provision hooks retain the legacy provision fallback", loaded.Context.YardName, group, source.Scope, source.Path, source.Line)
	}
	return fmt.Sprintf("yard %s: requested environment profiles [%s], from %s (%s scope, %s:%d)", loaded.Context.YardName, value, group, source.Scope, source.Path, source.Line)
}
