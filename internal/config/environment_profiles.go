package config

import (
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

// EnvironmentProfileSelection preserves authored intent while Environment carries
// the effective set used by hooks, resources and project admission.
type EnvironmentProfileSelection struct {
	Requested  []string          `json:"requested"`
	Effective  []string          `json:"effective"`
	Present    bool              `json:"present"`
	Provenance SettingResolution `json:"provenance"`
	// Unknown lists requested profiles this release does not ship, such as
	// ones a newer release added. They stay requested and are never effective.
	Unknown []string `json:"unknown,omitempty"`
}

func resolveEnvironmentProfiles(configDir, yard string, values map[string]string, catalog Catalog) (EnvironmentProfileSelection, error) {
	var unknown []string
	selection, err := resolveEnvironmentProfilesWith(yard, values, catalog, func(name string) (bool, error) {
		if UnknownProfile(configDir, catalog, name) {
			if !slices.Contains(unknown, name) {
				unknown = append(unknown, name)
			}
			return false, nil
		}
		eligible, _, err := EnvironmentProfileEligibility(configDir, yard, values, catalog, name)
		return eligible, err
	})
	selection.Unknown = unknown
	return selection, err
}

// UnknownProfile reports a well-formed profile name that this release neither
// ships, in its repository or the effective configuration directory, nor
// retired. Dropping it from a selection only removes what it would add: a
// present selection stays present, so no default profile replaces it.
func UnknownProfile(configDir string, catalog Catalog, name string) bool {
	if !domain.SafeName(name) || catalog.RetiredProfile(name) {
		return false
	}
	if _, err := profile.ResolvePackageInConfig(configDir, name); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	if catalog.root != "" {
		if _, err := profile.ResolvePackage(catalog.root, name); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

func resolveEnvironmentProfilesWith(yard string, values map[string]string, catalog Catalog, eligibleProfile func(string) (bool, error)) (EnvironmentProfileSelection, error) {
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
	// Every broken candidate package is reported, not only the first.
	var refusals []error
	for _, name := range candidates {
		eligible, err := eligibleProfile(name)
		if err != nil {
			refusals = append(refusals, err)
			continue
		}
		if eligible && !slices.Contains(selection.Effective, name) {
			selection.Effective = append(selection.Effective, name)
		}
	}
	return selection, domain.CollectRefusals(refusals...)
}

// EnvironmentProfileEligibility applies package policy and yard-role restrictions
// independently of authored selection, including descriptorless packages.
func EnvironmentProfileEligibility(configDir, yard string, values map[string]string, catalog Catalog, name string) (bool, string, error) {
	// This disabled profile can remain in authored selections after package removal.
	if catalog.RetiredProfile(name) {
		return false, "policy-disabled", nil
	}
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
	for _, name := range selection.Unknown {
		tracker.observeUnknownSelection("ENVIRONMENT_PROFILES", UnknownProfileInput, name)
	}
	tracker.environmentProfiles = selection
	values["ENVIRONMENT_PROFILES"] = strings.Join(selection.Effective, " ")
	return nil
}
