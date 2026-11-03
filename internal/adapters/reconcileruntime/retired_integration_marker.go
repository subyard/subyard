package reconcileruntime

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

// retiredIntegrationMarkerPrefix names the host-readable proof that an observation
// found this instance's retired package at its desired state. A stopped guest
// cannot be probed, so this proof alone decides whether init must boot it.
const retiredIntegrationMarkerPrefix = "user.subyard.retired_integration."

func pendingRetiredIntegrations(selected []string, definitions []profile.IntegrationDefinition, instance ports.InstanceInfo) []profile.IntegrationDefinition {
	pending := []profile.IntegrationDefinition{}
	for _, definition := range definitions {
		if !definition.Retired || definition.Lifecycle == nil || !slices.Contains(selected, definition.Name) {
			continue
		}
		if marker, _ := instance.EffectiveConfig(retiredIntegrationMarkerPrefix + definition.Name); marker == "" {
			pending = append(pending, definition)
		}
	}
	return pending
}

func (runtime Runtime) pendingRetiredIntegrations(instance ports.InstanceInfo) ([]profile.IntegrationDefinition, error) {
	// Role-forbidden yards have no integration reconcile that could prove retirement.
	if len(runtime.RetiredSelection) == 0 || runtime.environmentValue("ALLOWS_CODING_TOOLS") == "false" {
		return nil, nil
	}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return nil, err
	}
	return pendingRetiredIntegrations(runtime.RetiredSelection, definitions, instance), nil
}

// recordRetiredIntegrations runs only after an integration observation found every
// lifecycle at its desired state, including kept foreign state and already clean guests.
func (runtime Runtime) recordRetiredIntegrations(ctx context.Context) error {
	if len(runtime.RetiredSelection) == 0 {
		return nil
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil {
		return err
	}
	pending, err := runtime.pendingRetiredIntegrations(instance)
	if err != nil || len(pending) == 0 {
		return err
	}
	if runtime.ConfigWriter == nil {
		return fmt.Errorf("record retired integrations: instance config writer is required")
	}
	values := make(map[string]string, len(pending))
	for _, definition := range pending {
		values[retiredIntegrationMarkerPrefix+definition.Name] = "true"
	}
	if err := runtime.ConfigWriter.SetInstanceConfig(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, values); err != nil {
		return fmt.Errorf("record retired integrations: %w", err)
	}
	return nil
}

// RetiredIntegrationReminder is the host-only warning for a yard whose retired
// selection has no proof yet. It is empty once proven or where init cannot prove it.
func RetiredIntegrationReminder(loaded config.Loaded, instance ports.InstanceInfo) string {
	if !loaded.Integrations.AllowsCodingTools {
		return ""
	}
	names := []string{}
	for _, definition := range pendingRetiredIntegrations(loaded.Integrations.Retired, loaded.Catalog.Integrations(), instance) {
		names = append(names, definition.Name)
	}
	if len(names) == 0 {
		return ""
	}
	yard := loaded.Context.YardName
	return fmt.Sprintf("retired integration %s may still be installed in yard %s; run: yard -Y %s init", strings.Join(names, " "), yard, yard)
}
