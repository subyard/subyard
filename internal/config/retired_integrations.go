package config

import (
	"slices"

	"github.com/Subyard/Subyard/internal/profile"
)

// Retired IDs are persistent input compatibility, not executable packages.
// Supported installations may still carry these selections and assignments.
func retiredIntegrationInputs() []profile.IntegrationDefinition {
	settings := []string{}
	for _, suffix := range append(integrationMetadataSuffixes[:], "CLEANUP") {
		settings = append(settings, "AGENT_paseo_"+suffix)
	}
	return []profile.IntegrationDefinition{
		{SchemaVersion: 1, Name: "aiobserver", Retired: true, RetiredSettings: []string{"AI_OBSERVER_HOST_PORT"}},
		{SchemaVersion: 1, Name: "paseo", Retired: true, RetiredSettings: settings},
	}
}

// Historical semantic snapshots can already include a retired declaration.
func withRetiredIntegrationInputs(definitions []profile.IntegrationDefinition) []profile.IntegrationDefinition {
	for _, retired := range retiredIntegrationInputs() {
		if !slices.ContainsFunc(definitions, func(item profile.IntegrationDefinition) bool { return item.Name == retired.Name }) {
			definitions = append(definitions, retired)
		}
	}
	return definitions
}
