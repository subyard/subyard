package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// IntegrationSelection keeps authored roots separate from their dependency closure.
// Present distinguishes an absent request from an explicitly empty request.
type IntegrationSelection struct {
	Requested         []string            `json:"requested"`
	Effective         []string            `json:"effective"`
	Present           bool                `json:"present"`
	Provenance        SettingResolution   `json:"provenance"`
	DependencyReasons map[string][]string `json:"dependency_reasons"`
	AllowsCodingTools bool                `json:"allows_coding_tools"`
	Suppressed        bool                `json:"suppressed"`
	// Retired lists authored IDs ignored at load because their package is retired.
	Retired []string `json:"retired,omitempty"`
}

var integrationMetadataSuffixes = [...]string{"COMMAND", "CHECK", "HEALTH", "CONFIG", "CONFIG_DEST", "DEPENDS", "PERSIST", "PROJECTS_CHANGED", "PROVISION", "RULES", "RULES_DEST"}

func normalizeLegacySelection(name, value string) string {
	if name == "AGENTS" && strings.TrimSpace(value) == "none" {
		return ""
	}
	return value
}

// resolveLoadedIntegrations returns a role refusal separately from load errors,
// so the loader collects it with the other independent role refusals.
func resolveLoadedIntegrations(values environment, tracker *settingTracker) (refusal, err error) {
	requested := strings.Fields(values["CODING_TOOL_INTEGRATIONS"])
	var retired []string
	for _, definition := range tracker.catalog.Integrations() {
		if !definition.Retired {
			continue
		}
		filtered := requested[:0]
		for _, id := range requested {
			if id != definition.Name {
				filtered = append(filtered, id)
			} else if !slices.Contains(retired, id) {
				retired = append(retired, id)
			}
		}
		requested = filtered
		for _, name := range definition.RetiredSettings {
			delete(values, name)
			delete(tracker.assignments, name)
		}
	}
	// The role prohibition reads only authored assignments, so it is checked
	// before and independently of resolving the requested selection.
	refusal = integrationRoleRefusal(values, tracker)
	assignments := tracker.assignments["CODING_TOOL_INTEGRATIONS"]
	selection, err := tracker.catalog.ResolveIntegrationSelection(values, requested)
	if err != nil {
		return refusal, err
	}
	_, selection.Present = values["CODING_TOOL_INTEGRATIONS"]
	slices.Sort(retired)
	selection.Retired = retired
	if len(assignments) != 0 {
		assignment := assignments[len(assignments)-1]
		layer := tracker.layers[assignment.Layer]
		selection.Provenance = SettingResolution{
			Scope: layer.Scope, Role: layer.Role, Path: assignment.Path, Line: assignment.Line,
			Value: assignment.Value, Status: "effective", Detail: assignment.Detail,
		}
	}
	if !selection.AllowsCodingTools {
		selection.Suppressed = len(selection.Requested) != 0
		selection.Effective = []string{}
		selection.DependencyReasons = map[string][]string{}
	}
	values["CODING_TOOL_INTEGRATIONS"] = strings.Join(selection.Effective, " ")
	tracker.integrations = selection
	return refusal, nil
}

func codingToolsForbidden(values environment) bool {
	return values["ALLOWS_CODING_TOOLS"] == "false" || values["ALLOWS_CODING_TOOLS"] == "0"
}

func integrationRoleRefusal(values environment, tracker *settingTracker) error {
	if codingToolsForbidden(values) {
		for _, assignment := range tracker.assignments["CODING_TOOL_INTEGRATIONS"] {
			layer := tracker.layers[assignment.Layer]
			if (layer.Scope == "command" || (layer.Scope == "yard" && layer.Role == "scalar settings")) && len(strings.Fields(assignment.Value)) != 0 {
				return fmt.Errorf("%w in %s", domain.NewConfigRefusal(domain.ConfigRoleForbidsCodingTools), assignment.Path)
			}
		}
	}
	return nil
}

// ResolveIntegrationSelection validates roots and computes the existing ordered
// dependency closure. DependencyReasons records direct dependents for disable
// guards and status explanations; it is not another desired-state store.
func ResolveIntegrationSelection(values map[string]string, requested []string) (IntegrationSelection, error) {
	never := func(string) bool { return false }
	return resolveIntegrationSelection(values, requested, never, never)
}

// ResolveIntegrationSelection also names a refused integration when it is one
// this catalog ships; metadata settings alone never make a name printable.
func (settings Catalog) ResolveIntegrationSelection(values map[string]string, requested []string) (IntegrationSelection, error) {
	return resolveIntegrationSelection(values, requested, settings.ShippedIntegration, settings.RetiredIntegration)
}

func resolveIntegrationSelection(values map[string]string, requested []string, shipped, retired func(string) bool) (IntegrationSelection, error) {
	result := IntegrationSelection{
		Requested: append([]string{}, requested...), Effective: []string{}, Present: true,
		DependencyReasons: map[string][]string{}, AllowsCodingTools: values["ALLOWS_CODING_TOOLS"] != "false" && values["ALLOWS_CODING_TOOLS"] != "0",
	}
	if err := checkIntegrationNames(values, requested, shipped, retired); err != nil {
		return result, err
	}
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string) error
	visit = func(agent string) error {
		if visited[agent] {
			return nil
		}
		if visiting[agent] {
			return fmt.Errorf("agent dependency cycle at %q", agent)
		}
		visiting[agent] = true
		dependencies := strings.Fields(values["AGENT_"+agent+"_DEPENDS"])
		seenDependencies := make(map[string]bool, len(dependencies))
		for _, dependency := range dependencies {
			if seenDependencies[dependency] {
				return fmt.Errorf("duplicate dependency %q for agent %q", dependency, agent)
			}
			seenDependencies[dependency] = true
			if !knownIntegration(values, dependency) {
				return fmt.Errorf("unknown dependency %q for agent %q", dependency, agent)
			}
			result.DependencyReasons[dependency] = append(result.DependencyReasons[dependency], agent)
			if err := visit(dependency); err != nil {
				return err
			}
		}
		delete(visiting, agent)
		visited[agent] = true
		result.Effective = append(result.Effective, agent)
		return nil
	}
	for _, agent := range requested {
		if err := visit(agent); err != nil {
			return result, err
		}
	}
	return result, nil
}

func knownIntegration(values map[string]string, agent string) bool {
	if !domain.SafeName(agent) {
		return false
	}
	for _, suffix := range integrationMetadataSuffixes {
		if _, found := values["AGENT_"+agent+"_"+suffix]; found {
			return true
		}
	}
	return false
}

func checkIntegrationNames(values map[string]string, requested []string, shipped, retired func(string) bool) error {
	repeated := checkRepeatedIntegrations(requested, func(id string) bool { return shipped(id) && knownIntegration(values, id) })
	unknown := checkUnknownIntegrations(values, requested, retired, func(map[string]string, string) bool { return true })
	return domain.CollectRefusals(append(refusalCauses(repeated), refusalCauses(unknown)...)...)
}

// CheckRepeatedIntegrations checks list structure without reading metadata layers.
func (settings Catalog) CheckRepeatedIntegrations(requested []string) error {
	return checkRepeatedIntegrations(requested, settings.ShippedIntegration)
}

func checkRepeatedIntegrations(requested []string, shipped func(string) bool) error {
	var refusals []error
	selected := make(map[string]int, len(requested))
	for _, agent := range requested {
		if selected[agent]++; selected[agent] != 2 {
			continue
		}
		// Only shipped IDs may appear in the rendered diagnostic.
		if name, ok := domain.ConfigIntegrationName(agent); ok && shipped(agent) {
			refusals = append(refusals, domain.NewConfigRefusal(domain.ConfigDuplicateIntegration, name))
		} else {
			refusals = append(refusals, fmt.Errorf("%w %q", domain.NewConfigRefusal(domain.ConfigDuplicateUnknownIntegration), agent))
		}
	}
	return domain.CollectRefusals(refusals...)
}

func checkUnknownIntegrations(values map[string]string, requested []string, retired func(string) bool, metadataKnown func(map[string]string, string) bool) error {
	var refusals []error
	for _, agent := range requested {
		if !knownIntegration(values, agent) && !retired(agent) && metadataKnown(values, agent) {
			refusals = append(refusals, fmt.Errorf("%w %q", domain.NewConfigRefusal(domain.ConfigUnknownIntegration), agent))
		}
	}
	return domain.CollectRefusals(refusals...)
}

// WithIntegrationSelection prepares consumers for a planned requested set without
// writing it or promoting a temporary command override into persistent settings.
func WithIntegrationSelection(loaded Loaded, requested []string) (Loaded, error) {
	selection, err := ResolveIntegrationSelection(loaded.Environment, requested)
	if err != nil {
		return Loaded{}, err
	}
	if !selection.AllowsCodingTools && len(requested) != 0 {
		return Loaded{}, domain.NewConfigRefusal(domain.ConfigRoleForbidsCodingTools)
	}
	selection.Retired = loaded.Integrations.Retired
	loaded.Environment = cloneEnvironment(loaded.Environment)
	loaded.Integrations = selection
	loaded.Environment["CODING_TOOL_INTEGRATIONS"] = strings.Join(selection.Effective, " ")
	for _, resolution := range loaded.Settings["HOST_LINKS"].Resolutions {
		if resolution.Status != "effective" || resolution.Scope != "default" {
			continue
		}
		var links strings.Builder
		for _, id := range selection.Effective {
			links.WriteString(loaded.Environment["AGENT_"+id+"_PERSIST"])
		}
		loaded.Environment["HOST_LINKS"] = links.String()
		loaded.Environment["INTEGRATION_HOST_LINKS"] = links.String()
	}
	return loaded, nil
}

// ValidateYardTemplateIntegrations checks a template transition against the
// existing explicit requests without writing a candidate registration. Inherited
// host/shared selections remain suppressible by the destination role.
func ValidateYardTemplateIntegrations(loaded Loaded, template string) error {
	if template == "" {
		return nil
	}
	if !domain.SafeName(template) {
		return fmt.Errorf("%w %q", domain.NewConfigRefusal(domain.ConfigYardTemplateInvalid), template)
	}
	path := filepath.Join(loaded.Context.Paths.ConfigDir, "yards", "profiles", template+".env")
	values := cloneEnvironment(loaded.Environment)
	delete(values, "ALLOWS_CODING_TOOLS")
	if err := loaded.Catalog.applyEnvFileValidated(path, values, ScopeShipped, false, nil); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w %q", domain.NewConfigRefusal(domain.ConfigUnknownYardTemplate), template)
		}
		return err
	}
	if values["ALLOWS_CODING_TOOLS"] != "false" {
		return nil
	}
	for _, resolution := range loaded.Settings["CODING_TOOL_INTEGRATIONS"].Resolutions {
		if resolution.Status == "unset" || len(strings.Fields(resolution.Value)) == 0 {
			continue
		}
		if resolution.Scope == "command" || (resolution.Scope == "yard" && resolution.Role == "scalar settings") {
			return domain.NewConfigRefusal(domain.ConfigTemplateForbidsCodingTools)
		}
	}
	return nil
}
