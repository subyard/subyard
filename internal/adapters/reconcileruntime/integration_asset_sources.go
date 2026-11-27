package reconcileruntime

import (
	"path/filepath"
	"slices"
	"strings"
)

func integrationProjectsDispatcherSource(root string) string {
	return filepath.Join(root, "config", "projects-changed.sh")
}

// IntegrationAssetSources lists repository inputs used by the existing desired
// artifact inventory. Package inputs include metadata, all approval variants,
// provisioning and lifecycle sources, including their declared artifact sources.
// Operator-selected external files remain the operator's inputs, not release assets.
func (runtime Runtime) IntegrationAssetSources() ([]string, error) {
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil, err
	}
	sources := []string{integrationProjectsDispatcherSource(runtime.RepositoryRoot)}
	for _, definition := range definitions {
		sources = append(sources, definition.ValidationInputs()...)
	}
	files, err := runtime.guestConfigFiles()
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if file.source != "" {
			sources = append(sources, file.source)
		}
	}
	for _, id := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		if source := runtime.environmentValue("AGENT_" + id + "_PROVISION"); source != "" {
			sources = append(sources, source)
		}
	}
	if source := runtime.environmentValue("CCUSAGE_PROVISION"); source != "" {
		sources = append(sources, source)
	}
	slices.Sort(sources)
	return slices.Compact(sources), nil
}
