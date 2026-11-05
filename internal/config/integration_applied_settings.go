package config

import "strings"

// IntegrationInitSetting identifies non-secret inputs to integration runtime
// reconciliation. File settings contribute their configured path, never contents.
func (catalog Catalog) IntegrationInitSetting(name string) bool {
	definition, ok := catalog.LookupSetting(name)
	return ok && definition.Application == SettingYardInit && !definition.Sensitive &&
		(definition.Owner == "agent-integration" || definition.Owner == "coding-tool-integration" || definition.Owner == "host-files" ||
			strings.HasPrefix(definition.Owner, "integration:") || name == "HOST_LINKS" || name == "ALLOWS_HOST_ACCESS" || name == "ALLOWS_CODING_TOOLS" || name == "DEV_USER" || name == "DEV_UID")
}

func (catalog Catalog) IntegrationInitSettings(values map[string]string) map[string]string {
	// A non-nil map is a complete snapshot of this closed input domain. Missing
	// keys record absence; nil remains the legacy "baseline unavailable" state.
	result := map[string]string{}
	for name, value := range values {
		if catalog.IntegrationInitSetting(name) && name != "CODING_TOOL_INTEGRATIONS" {
			result[name] = value
		}
	}
	if value, ok := values["INTEGRATION_HOST_LINKS"]; ok {
		result["INTEGRATION_HOST_LINKS"] = value
	}
	return result
}

// WithAppliedIntegrationInitSettings projects the complete applied input domain
// without allowing newly authored absent-to-present fields to enter inspection.
func (catalog Catalog) WithAppliedIntegrationInitSettings(values, applied map[string]string) map[string]string {
	result := make(map[string]string, len(values)+len(applied))
	for name, value := range values {
		if applied != nil && (catalog.IntegrationInitSetting(name) && name != "CODING_TOOL_INTEGRATIONS" || name == "INTEGRATION_HOST_LINKS") {
			continue
		}
		result[name] = value
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
