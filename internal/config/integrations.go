package config

import (
	"fmt"
	"os"
	"path/filepath"
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
}

func normalizeLegacySelection(name, value string) string {
	if name == "AGENTS" && strings.TrimSpace(value) == "none" {
		return ""
	}
	return value
}

func resolveLoadedIntegrations(values environment, tracker *settingTracker) error {
	requested := strings.Fields(values["CODING_TOOL_INTEGRATIONS"])
	selection, err := ResolveIntegrationSelection(values, requested)
	if err != nil {
		return err
	}
	_, selection.Present = values["CODING_TOOL_INTEGRATIONS"]
	assignments := tracker.assignments["CODING_TOOL_INTEGRATIONS"]
	if len(assignments) != 0 {
		assignment := assignments[len(assignments)-1]
		layer := tracker.layers[assignment.Layer]
		selection.Provenance = SettingResolution{
			Scope: layer.Scope, Role: layer.Role, Path: assignment.Path, Line: assignment.Line,
			Value: assignment.Value, Status: "effective", Detail: assignment.Detail,
		}
	}
	if !selection.AllowsCodingTools {
		// An inherited default can be suppressed; a concrete forbidden request cannot.
		for _, assignment := range assignments {
			layer := tracker.layers[assignment.Layer]
			if (layer.Scope == "command" || (layer.Scope == "yard" && layer.Role == "scalar settings")) && len(strings.Fields(assignment.Value)) != 0 {
				return fmt.Errorf("yard role forbids non-empty CODING_TOOL_INTEGRATIONS in %s", assignment.Path)
			}
		}
		selection.Suppressed = len(selection.Requested) != 0
		selection.Effective = []string{}
		selection.DependencyReasons = map[string][]string{}
	}
	values["CODING_TOOL_INTEGRATIONS"] = strings.Join(selection.Effective, " ")
	tracker.integrations = selection
	return nil
}

// ResolveIntegrationSelection validates roots and computes the existing ordered
// dependency closure. DependencyReasons records direct dependents for disable
// guards and status explanations; it is not another desired-state store.
func ResolveIntegrationSelection(values map[string]string, requested []string) (IntegrationSelection, error) {
	result := IntegrationSelection{
		Requested: append([]string{}, requested...), Effective: []string{}, Present: true,
		DependencyReasons: map[string][]string{}, AllowsCodingTools: values["ALLOWS_CODING_TOOLS"] != "false" && values["ALLOWS_CODING_TOOLS"] != "0",
	}
	selected := make(map[string]bool, len(requested))
	for _, agent := range requested {
		if selected[agent] {
			return result, fmt.Errorf("duplicate agent %q", agent)
		}
		selected[agent] = true
	}
	knownAgent := func(agent string) bool {
		for _, suffix := range []string{"COMMAND", "CHECK", "CONFIG", "CONFIG_DEST", "DEPENDS", "PERSIST", "PROJECTS_CHANGED", "PROVISION", "RULES", "RULES_DEST"} {
			if _, found := values["AGENT_"+agent+"_"+suffix]; found {
				return true
			}
		}
		return false
	}
	for _, agent := range requested {
		if !domain.SafeName(agent) || !knownAgent(agent) {
			return result, fmt.Errorf("unknown integration %q", agent)
		}
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
			if !domain.SafeName(dependency) || !knownAgent(dependency) {
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

// WithIntegrationSelection prepares consumers for a planned requested set without
// writing it or promoting a temporary command override into persistent settings.
func WithIntegrationSelection(loaded Loaded, requested []string) (Loaded, error) {
	selection, err := ResolveIntegrationSelection(loaded.Environment, requested)
	if err != nil {
		return Loaded{}, err
	}
	if !selection.AllowsCodingTools && len(requested) != 0 {
		return Loaded{}, fmt.Errorf("yard role forbids non-empty CODING_TOOL_INTEGRATIONS")
	}
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
		return fmt.Errorf("invalid YARD_TEMPLATE %q", template)
	}
	path := filepath.Join(loaded.Context.Paths.ConfigDir, "yards", "profiles", template+".env")
	values := cloneEnvironment(loaded.Environment)
	delete(values, "ALLOWS_CODING_TOOLS")
	if err := applyEnvFileValidated(path, values, ScopeShipped, false, nil); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("unknown YARD_TEMPLATE %q", template)
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
			return fmt.Errorf("selected yard template forbids coding tool integrations; clear the yard's CODING_TOOL_INTEGRATIONS and remove temporary selection overrides before changing YARD_TEMPLATE")
		}
	}
	return nil
}
