package ownerapi

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

type ProfileSelection struct {
	Value      string              `json:"value"`
	Provenance []SettingProvenance `json:"provenance"`
}

type Profile struct {
	Name              string   `json:"name"`
	HasYardPreset     bool     `json:"hasYardPreset"`
	DescriptorVersion *int     `json:"descriptorVersion,omitempty"`
	Selected          bool     `json:"selected"`
	Provisionable     bool     `json:"provisionable"`
	ProvisionScope    string   `json:"provisionScope"`
	Eligible          bool     `json:"eligible"`
	Eligibility       string   `json:"eligibility"`
	Convergence       string   `json:"convergence"`
	Diagnostic        string   `json:"diagnostic,omitempty"`
	Resources         []string `json:"resources"`
}

type ProfileList struct {
	SchemaVersion int              `json:"schemaVersion"`
	YardName      string           `json:"yardName"`
	Selection     ProfileSelection `json:"selection"`
	Profiles      []Profile        `json:"profiles"`
}

func (service Service) ProfileList(ctx context.Context) (ProfileList, error) {
	loaded := service.Loaded
	result := ProfileList{SchemaVersion: 1, YardName: loaded.Context.YardName,
		Selection: ProfileSelection{Value: loaded.Environment["ENVIRONMENT_PROFILES"],
			Provenance: settingProvenance(loaded.Settings["ENVIRONMENT_PROFILES"], false)},
		Profiles: []Profile{}}
	hooks, err := service.ProvisionProfiles()
	if err != nil {
		return result, &Error{Code: "profile_catalog_invalid", Message: "shipped provision catalog is invalid"}
	}
	entries, err := os.ReadDir(filepath.Join(service.RepositoryRoot, "config", "profiles"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, &Error{Code: "profile_catalog_invalid", Message: "shipped profile catalog is unavailable"}
	}
	declarations := loaded.Catalog.Profiles()
	selection, explicit := loaded.Environment["ENVIRONMENT_PROFILES"]
	for _, directory := range entries {
		name := directory.Name()
		if !directory.IsDir() || !domain.SafeName(name) {
			continue
		}
		scope, err := config.ProfileProvisionScope(loaded.Context.Paths.ConfigDir, name)
		if err != nil {
			return result, &Error{Code: "profile_catalog_invalid", Message: "shipped provision scope is invalid"}
		}
		entry := Profile{Name: name, Provisionable: slices.Contains(hooks, name), ProvisionScope: scope,
			Selected: slices.Contains(strings.Fields(selection), name), Eligible: true,
			Eligibility: "allowed", Convergence: "not-applicable", Resources: []string{}}
		preset, presetErr := os.Lstat(filepath.Join(service.RepositoryRoot, "config", "profiles", name, "yard.env"))
		entry.HasYardPreset = presetErr == nil && preset.Mode().IsRegular() && preset.Mode().Perm()&0o022 == 0 && preset.Size() <= 8<<20
		for _, declaration := range declarations {
			if declaration.Name == name {
				version := declaration.SchemaVersion
				entry.DescriptorVersion = &version
				entry.Selected = declaration.Selected(loaded.Context.YardName, loaded.Environment)
			}
		}
		if !explicit && entry.DescriptorVersion == nil {
			entry.Selected = entry.Provisionable
		}
		exclusive := loaded.Environment["EXCLUSIVE_ENVIRONMENT_PROFILE"]
		if exclusive != "" && exclusive != name {
			entry.Eligible, entry.Eligibility = false, "exclusive-role"
		} else if scope == "dedicated" && (exclusive != name || loaded.Context.YardName == "default") {
			entry.Eligible, entry.Eligibility = false, "dedicated-role-required"
		}
		for _, resource := range service.Resources.Definitions() {
			if resource.Profile == name {
				entry.Resources = append(entry.Resources, resource.Command)
			}
		}
		sort.Strings(entry.Resources)
		if entry.Provisionable {
			entry.Convergence = "unknown"
			entry.Diagnostic = "not-selected"
			if !entry.Eligible {
				entry.Diagnostic = "role-restricted"
			} else if entry.Selected {
				entry.Diagnostic = "yard-unavailable"
			}
		}
		result.Profiles = append(result.Profiles, entry)
	}
	// Inspect power once. A read never starts a stopped yard or runs apply hooks.
	needsCheck := slices.ContainsFunc(result.Profiles, func(entry Profile) bool { return entry.Selected && entry.Eligible && entry.Provisionable })
	if !needsCheck {
		return result, nil
	}
	incus := service.Instances
	instance, err := incus.Instance(ctx, loaded.Context.IncusProject, loaded.Context.YardInstanceName)
	if err != nil || !strings.EqualFold(instance.Status, "running") {
		if err == nil {
			for index := range result.Profiles {
				entry := &result.Profiles[index]
				if entry.Selected && entry.Eligible && entry.Provisionable {
					entry.Diagnostic = "yard-not-running"
				}
			}
		}
		return result, ctx.Err()
	}
	check := service.ProfileCheck()
	if check.Runner == nil {
		return result, nil
	}
	runner := check.Runner
	for index := range result.Profiles {
		entry := &result.Profiles[index]
		if !entry.Selected || !entry.Eligible || !entry.Provisionable {
			continue
		}
		entry.Diagnostic = "check-unavailable"
		checked, output, err := runner.Run(ctx, domain.AdapterRequest{
			Schema: shelladapter.ProtocolSchema, OperationID: service.OperationID,
			Adapter: "provision", Action: "profile-check", Arguments: []string{"--check", entry.Name},
			Context: maps.Clone(check.Context),
		}, nil)
		if err != nil || checked.Status != "ok" {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			continue
		}
		switch strings.TrimSpace(output) {
		case "converged":
			entry.Convergence = "current"
			entry.Diagnostic = ""
		case "changed":
			entry.Convergence = "changes-required"
			entry.Diagnostic = ""
		}
	}
	return result, nil
}
