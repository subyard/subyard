package config

import (
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

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
		(definition.Owner == "agent-integration" || definition.Owner == "coding-tool-integration" ||
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
	// Snapshots record only yes and no, so older runtimes can still read the
	// default. A snapshot from before the setting also reads as prompt: release
	// activation reprovisions those agents with prompt, since their hooks changed.
	result["DEV_SUDO"] = "prompt"
	for name, value := range applied {
		if catalog.AppliedIntegrationInitSetting(name) {
			result[name] = value
		}
	}
	return result
}

// AppliedIntegrationInitSetting reports whether an applied snapshot name is an
// input this release reads. A snapshot written by another release can record
// names this one does not classify, such as a newer release's settings.
func (catalog Catalog) AppliedIntegrationInitSetting(name string) bool {
	return name == "INTEGRATION_HOST_LINKS" || catalog.IntegrationInitSetting(name) && name != "CODING_TOOL_INTEGRATIONS"
}

// ReadableAppliedIntegrationInitSettings keeps the snapshot names this release
// reads. Other names are ignored, never validated, projected or rendered; the
// next capture replaces them. A nil snapshot stays nil (no baseline), and a
// snapshot of only other names reads as complete and empty.
func (catalog Catalog) ReadableAppliedIntegrationInitSettings(applied map[string]string) map[string]string {
	if applied == nil {
		return nil
	}
	result := make(map[string]string, len(applied))
	for name, value := range applied {
		if catalog.AppliedIntegrationInitSetting(name) {
			result[name] = value
		}
	}
	return result
}

// InheritedReleaseRoot matches a path below an inherited release root handle.
var InheritedReleaseRoot = regexp.MustCompile(`^/proc/[0-9]+/fd/[0-9]+/(.+)$`)

// ShippedReleasePath returns the shipped config path inside a release root:
// an inherited /proc/<pid>/fd/<n> handle or a directory under releases/<id>.
func ShippedReleasePath(path string) (string, bool) {
	path = filepath.Clean(path)
	relative := ""
	if match := InheritedReleaseRoot.FindStringSubmatch(path); match != nil {
		relative = match[1]
	} else {
		parts := strings.Split(path, "/")
		for index := len(parts) - 3; index > 0; index-- {
			if parts[index] == "releases" && parts[index+1] != "" {
				relative = strings.Join(parts[index+2:], "/")
				break
			}
		}
	}
	if relative == "" || !strings.HasPrefix(relative, "config/") || !filepath.IsLocal(relative) {
		return "", false
	}
	return relative, true
}

// ReadableAppliedIntegrations drops the applied integrations that another
// release shipped and this one does not: an ID this release neither ships nor
// retired, that the desired values do not define, and whose recorded
// AGENT_<id>_ metadata is absent or names a shipped release source. It goes
// with that metadata, so its recorded command and sources are never read. An
// applied custom integration, whose metadata names no release source, stays
// installed evidence after it is removed from the desired values. A malformed
// ID stays and is refused by selection resolution.
func (catalog Catalog) ReadableAppliedIntegrations(desired map[string]string, selected []string, applied map[string]string) ([]string, map[string]string) {
	shippedElsewhere := func(id string) bool {
		if !catalog.UnknownIntegration(desired, id) {
			return false
		}
		recorded := false
		for _, suffix := range integrationMetadataSuffixes {
			value, ok := applied["AGENT_"+id+"_"+suffix]
			if !ok {
				continue
			}
			recorded = true
			if _, shipped := ShippedReleasePath(value); shipped {
				return true
			}
		}
		return !recorded
	}
	var dropped []string
	kept := slices.DeleteFunc(slices.Clone(selected), func(id string) bool {
		drop := shippedElsewhere(id)
		if drop {
			dropped = append(dropped, id)
		}
		return drop
	})
	if len(dropped) == 0 || applied == nil {
		return kept, applied
	}
	readable := maps.Clone(applied)
	owned := map[string]bool{}
	for _, id := range dropped {
		for _, link := range strings.Fields(applied["AGENT_"+id+"_PERSIST"]) {
			owned[link] = true
		}
		for _, suffix := range integrationMetadataSuffixes {
			delete(readable, "AGENT_"+id+"_"+suffix)
		}
	}
	for _, id := range kept {
		for _, link := range strings.Fields(applied["AGENT_"+id+"_PERSIST"]) {
			delete(owned, link)
		}
	}
	// The dropped integrations' persist links leave the recorded link sets too;
	// a link without a selected owner is otherwise refused.
	for _, name := range []string{"HOST_LINKS", "INTEGRATION_HOST_LINKS"} {
		value, ok := readable[name]
		if !ok {
			continue
		}
		links := strings.Fields(value)
		if remaining := slices.DeleteFunc(slices.Clone(links), func(link string) bool { return owned[link] }); len(remaining) != len(links) {
			readable[name] = strings.Join(remaining, " ")
		}
	}
	return kept, readable
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

// ImplicitAppliedSetting reports the default values a snapshot omits, so readers
// from before each setting can still read it: Git prompt and DEV_SUDO prompt.
func ImplicitAppliedSetting(name, value string) bool {
	return (name == "CODING_TOOL_GIT_APPROVAL" || name == "DEV_SUDO") && (value == "" || value == "prompt")
}
