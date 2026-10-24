package config

import (
	"slices"
	"strings"
)

// EnvironmentProfileSelection preserves authored intent while Environment carries
// the effective set used by hooks, resources and project admission.
type EnvironmentProfileSelection struct {
	Requested  []string          `json:"requested"`
	Effective  []string          `json:"effective"`
	Present    bool              `json:"present"`
	Provenance SettingResolution `json:"provenance"`
}

func resolveEnvironmentProfiles(configDir, yard string, values map[string]string, catalog Catalog) (EnvironmentProfileSelection, error) {
	value, present := values["ENVIRONMENT_PROFILES"]
	selection := EnvironmentProfileSelection{Requested: strings.Fields(value), Effective: []string{}, Present: present}
	candidates := slices.Clone(selection.Requested)
	if !present {
		for _, definition := range catalog.Profiles() {
			if slices.Contains(definition.DefaultYards, yard) {
				candidates = append(candidates, definition.Name)
			}
		}
	}
	for _, name := range candidates {
		eligible, _, err := EnvironmentProfileEligibility(configDir, yard, values, catalog, name)
		if err != nil {
			return selection, err
		}
		if eligible && !slices.Contains(selection.Effective, name) {
			selection.Effective = append(selection.Effective, name)
		}
	}
	return selection, nil
}

// EnvironmentProfileEligibility applies package policy and yard-role restrictions
// independently of authored selection, including descriptorless packages.
func EnvironmentProfileEligibility(configDir, yard string, values map[string]string, catalog Catalog, name string) (bool, string, error) {
	scope, err := ProfileProvisionScope(configDir, name)
	if err != nil {
		return false, "", err
	}
	exclusive := values["EXCLUSIVE_ENVIRONMENT_PROFILE"]
	if exclusive != "" && exclusive != name {
		return false, "exclusive-role", nil
	}
	if scope == "dedicated" && (exclusive != name || yard == "default") {
		return false, "dedicated-role-required", nil
	}
	for _, definition := range catalog.Profiles() {
		if definition.Name != name {
			continue
		}
		if definition.Disabled {
			return false, "policy-disabled", nil
		}
		if definition.LifecycleOnly {
			return false, "lifecycle-only", nil
		}
		for key, disabled := range definition.DisabledWhen {
			if actual, ok := values[key]; ok && actual == disabled {
				return false, "policy-disabled", nil
			}
		}
	}
	return true, "allowed", nil
}

func resolveLoadedEnvironmentProfiles(configDir, yard string, values environment, tracker *settingTracker) error {
	selection, err := resolveEnvironmentProfiles(configDir, yard, values, tracker.catalog)
	if err != nil {
		return err
	}
	assignments := tracker.assignments["ENVIRONMENT_PROFILES"]
	if len(assignments) != 0 {
		assignment := assignments[len(assignments)-1]
		layer := tracker.layers[assignment.Layer]
		selection.Provenance = SettingResolution{Scope: layer.Scope, Role: layer.Role, Path: assignment.Path,
			Line: assignment.Line, Value: assignment.Value, Status: "effective", Detail: assignment.Detail}
	}
	tracker.environmentProfiles = selection
	values["ENVIRONMENT_PROFILES"] = strings.Join(selection.Effective, " ")
	return nil
}
