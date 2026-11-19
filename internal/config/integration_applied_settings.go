package config

import "strings"

// IntegrationInitSetting identifies non-secret inputs to integration runtime
// reconciliation. File settings contribute their configured path, never contents.
func (catalog Catalog) IntegrationInitSetting(name string) bool {
	definition, ok := catalog.LookupSetting(name)
	return ok && !definition.Sensitive && integrationInitDefinition(name, definition)
}

func (catalog Catalog) IntegrationDiagnosticSetting(name string) bool {
	definition, ok := catalog.LookupSetting(name)
	return ok && integrationInitDefinition(name, definition)
}

func integrationInitDefinition(name string, definition SettingDefinition) bool {
	return definition.Application == SettingYardInit &&
		(definition.Owner == "agent-integration" || definition.Owner == "coding-tool-integration" || definition.Owner == "host-files" ||
			strings.HasPrefix(definition.Owner, "integration:") || name == "HOST_LINKS" || name == "ALLOWS_HOST_ACCESS" || name == "ALLOWS_CODING_TOOLS" || name == "DEV_USER" || name == "DEV_UID" || name == "DEV_SUDO")
}

func (catalog Catalog) IntegrationInitSettings(values map[string]string) map[string]string {
	// A non-nil map is a complete snapshot of this closed input domain. Missing
	// keys record absence; nil remains the legacy "baseline unavailable" state.
	result := map[string]string{}
	for name, value := range values {
		if catalog.IntegrationInitSetting(name) && name != "CODING_TOOL_INTEGRATIONS" && !ImplicitAppliedSetting(name, value) {
			result[name] = value
		}
	}
	if value, ok := values["INTEGRATION_HOST_LINKS"]; ok {
		result["INTEGRATION_HOST_LINKS"] = value
	}
	return result
}

// WithAppliedIntegrationInitSettings projects the complete applied input domain
// for an installed inventory. Legacy inventories retain other unknown inputs.
func (catalog Catalog) WithAppliedIntegrationInitSettings(values, applied map[string]string) map[string]string {
	result := make(map[string]string, len(values)+len(applied))
	for name, value := range values {
		if applied != nil && (catalog.IntegrationInitSetting(name) && name != "CODING_TOOL_INTEGRATIONS" || name == "INTEGRATION_HOST_LINKS") {
			continue
		}
		result[name] = value
	}
	// Both legacy inventories without a snapshot and snapshots that omit the
	// setting record the installed prompt mode. Explicit off overrides it below.
	result["CODING_TOOL_GIT_APPROVAL"] = "prompt"
	// Only prompt changes agent policy, so snapshots record only prompt and the
	// grant keeps its desired value. No earlier runtime installed sudo prompts.
	if value := values["DEV_SUDO"]; value == "no" {
		result["DEV_SUDO"] = value
	} else {
		result["DEV_SUDO"] = "yes"
	}
	for name, value := range applied {
		result[name] = value
	}
	return result
}

func (catalog Catalog) ValidateIntegrationInitSettings(values map[string]string) error {
	for name, value := range values {
		if name == "INTEGRATION_HOST_LINKS" {
			if err := catalog.ValidateSetting(ScopeShipped, "HOST_LINKS", value, false); err != nil {
				return err
			}
			continue
		}
		if !catalog.IntegrationInitSetting(name) || name == "CODING_TOOL_INTEGRATIONS" {
			return sourceError("", 0, "applied integration settings", "installed configuration", "contains an unsupported field", nil)
		}
		definition, _ := catalog.LookupSetting(name)
		// This validates retained values, not a new authoring request. A package
		// may declare host-only inputs without a shipped default.
		if len(definition.Scopes) == 0 {
			return sourceError("", 0, "applied integration settings", "installed configuration", "has no permitted input scope", nil)
		}
		if err := catalog.ValidateSetting(definition.Scopes[0], name, value, false); err != nil {
			return err
		}
	}
	return nil
}

// ImplicitAppliedSetting reports the values a snapshot omits: the legacy
// encoding for Git prompt, which readers from before the setting can still
// read, and DEV_SUDO yes and no, which install the same agent policy.
func ImplicitAppliedSetting(name, value string) bool {
	return name == "CODING_TOOL_GIT_APPROVAL" && (value == "" || value == "prompt") ||
		name == "DEV_SUDO" && (value == "yes" || value == "no")
}
