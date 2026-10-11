package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

// Integration install includes the existing bounded image pull and package work.
const integrationLifecycleApplyLimit = 22 * time.Minute

func (runtime Runtime) integrationDefinitions() ([]profile.IntegrationDefinition, error) {
	if runtime.Integrations != nil {
		return slices.Clone(runtime.Integrations), nil
	}
	return profile.LoadIntegrations(runtime.RepositoryRoot)
}
func (runtime Runtime) integrationPackageBinding(definition profile.IntegrationDefinition) (string, error) {
	source, err := definition.SourceBinding()
	if err != nil {
		return "", err
	}
	values := map[string]string{}
	for _, setting := range definition.Settings {
		values[setting.Name] = runtime.environmentValue(setting.Name)
	}
	for _, input := range definition.Inputs {
		values[input.Name] = input.Value
	}
	for _, suffix := range []string{"PROVISION", "CLEANUP"} {
		path := runtime.environmentValue("AGENT_" + definition.Name + "_" + suffix)
		if path == "" {
			continue
		}
		data, err := (config.MaterializedAsset{Source: path}).ReadSource()
		if err != nil {
			return "", err
		}
		values["AGENT_"+definition.Name+"_"+suffix] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	payload, _ := json.Marshal(struct {
		Source string
		Values map[string]string
	}{source, values})
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}
func (runtime Runtime) integrationSourceBinding() (string, error) {
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return "", err
	}
	if len(definitions) == 0 {
		// Retain the original scope grammar when no package contract is declared.
		return "", nil
	}
	sources := map[string]string{}
	for _, definition := range definitions {
		binding, err := runtime.integrationPackageBinding(definition)
		if err != nil {
			return "", err
		}
		sources[definition.Name] = binding
	}
	payload, _ := json.Marshal(sources)
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}
func (runtime Runtime) integrationLifecycleEnvironment(definition profile.IntegrationDefinition) ([]string, error) {
	binding, err := runtime.integrationPackageBinding(definition)
	if err != nil {
		return nil, err
	}
	environment := slices.Clone(runtime.Environment)
	overrides := map[string]string{"SUBYARD_INTEGRATION_SOURCE_BINDING": binding, "SUBYARD_INTEGRATION_SELECTED": "0", "INCUS_PROJECT": runtime.Yard.IncusProject, "YARD_INSTANCE_NAME": runtime.Yard.YardInstanceName, "YARD_KIND": string(runtime.Yard.YardKind), "DEV_USER": runtime.devUser(), "DEV_UID": runtime.environmentDefault("DEV_UID", "1000"), "SUBYARD_REPOSITORY_ROOT": runtime.RepositoryRoot}
	if slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), definition.Name) {
		overrides["SUBYARD_INTEGRATION_SELECTED"] = "1"
	}
	for _, input := range definition.Inputs {
		overrides[input.Name] = input.Value
	}
	environment = slices.DeleteFunc(environment, func(value string) bool { name, _, _ := strings.Cut(value, "="); _, ok := overrides[name]; return ok })
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}
	return environment, nil
}
func (runtime Runtime) observeIntegrationLifecycle(ctx context.Context, definition profile.IntegrationDefinition) (ports.RuntimeObservation, error) {
	environment, err := runtime.integrationLifecycleEnvironment(definition)
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	stdout, _, err := runtime.runShippedHandler(ctx, profile.Definition{Root: definition.Root}, definition.Lifecycle.Handler, environment, profileRuntimeObserveLimit, "observe")
	if err != nil {
		return ports.RuntimeObservation{}, fmt.Errorf("observe integration %s lifecycle: %w", definition.Name, err)
	}
	observation, err := decodeProfileRuntimeObservation(stdout)
	selected := slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), definition.Name)
	if err == nil && (observation.State == ports.RuntimeStateDeferred || observation.State == ports.RuntimeStateAbsent && selected || observation.HookBinding != "" || observation.State == ports.RuntimeStateStale && (observation.Actual == "" || observation.Actual == observation.Desired)) {
		err = errors.New("invalid integration lifecycle state")
	}
	return observation, err
}
func (runtime Runtime) integrationLifecycleFact(definition profile.IntegrationDefinition, observation ports.RuntimeObservation) integrationFact {
	fact := integrationFact{Key: "lifecycle:" + definition.Name, Observed: observation.Actual, Desired: observation.Desired, AtDesired: observation.State == ports.RuntimeStateCurrent || observation.State == ports.RuntimeStateAbsent}
	if !slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), definition.Name) {
		fact.Desired = "absent"
		fact.Observed = observation.Actual + ":" + observation.Desired
	}
	return fact
}
func (runtime Runtime) applyIntegrationLifecycles(ctx context.Context, plan IntegrationPlan) error {
	sources, err := runtime.integrationSourceBinding()
	if err != nil {
		return err
	}
	if sources != plan.sources {
		return fmt.Errorf("%w: integration package sources changed", domain.ErrPlanStale)
	}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return err
	}
	for _, definition := range definitions {
		if definition.Lifecycle == nil {
			continue
		}
		observation, err := runtime.observeIntegrationLifecycle(ctx, definition)
		if err != nil {
			return err
		}
		if observation.State == ports.RuntimeStateAbsent || observation.State == ports.RuntimeStateCurrent {
			continue
		}
		approved := false
		nativeFact := runtime.integrationLifecycleFact(definition, observation)
		for _, fact := range plan.facts {
			if fact.Key == nativeFact.Key && fact.Observed == nativeFact.Observed && fact.Desired == nativeFact.Desired && !fact.AtDesired {
				approved = true
			}
		}
		if !approved {
			return fmt.Errorf("%w: integration lifecycle changed", domain.ErrPlanStale)
		}
		environment, err := runtime.integrationLifecycleEnvironment(definition)
		if err != nil {
			return err
		}
		operationID := runtime.environmentValue("SUBYARD_OPERATION_ID")
		if !domain.SafeID(operationID) {
			return errors.New("integration lifecycle requires a valid operation ID")
		}
		stdout, stderr, err := runtime.runShippedHandler(ctx, profile.Definition{Root: definition.Root}, definition.Lifecycle.Handler, environment, integrationLifecycleApplyLimit, "apply", operationID, observation.Actual, observation.Desired)
		if runtime.Stderr != nil && len(stderr) != 0 {
			_, _ = runtime.Stderr.Write(stderr)
		}
		if err != nil {
			return err
		}
		applied, err := decodeProfileRuntimeObservation(stdout)
		if err != nil {
			return err
		}
		selected := slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), definition.Name)
		if !(applied.State == ports.RuntimeStateAbsent && !selected) && (applied.State != ports.RuntimeStateCurrent || applied.Actual != observation.Desired || applied.Desired != observation.Desired) {
			return errors.New("integration lifecycle apply did not converge")
		}
		verified, err := runtime.observeIntegrationLifecycle(ctx, definition)
		if err != nil {
			return err
		}
		if verified != applied {
			return errors.New("integration lifecycle independent verification failed")
		}
	}
	return nil
}
