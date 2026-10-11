package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
	return runtime.integrationSourceBindingPhase("inspect")
}
func (runtime Runtime) integrationSourceBindingPhase(phase string) (string, error) {
	binding, _, err := runtime.integrationSourceBindingsPhase(phase)
	return binding, err
}
func (runtime Runtime) integrationSourceBindingsPhase(phase string) (string, map[string]string, error) {
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return "", nil, err
	}
	if len(definitions) == 0 {
		// Retain the original scope grammar when no package contract is declared.
		return "", nil, nil
	}
	sources := map[string]string{}
	for _, definition := range definitions {
		binding, err := runtime.integrationPackageBinding(definition)
		if err != nil {
			return "", nil, runtime.integrationDiagnostic(err, definition, phase, "native-unavailable")
		}
		sources[definition.Name] = binding
	}
	payload, _ := json.Marshal(sources)
	return fmt.Sprintf("%x", sha256.Sum256(payload)), sources, nil
}
func (runtime Runtime) integrationLifecycleEnvironment(definition profile.IntegrationDefinition) ([]string, error) {
	binding, err := runtime.integrationPackageBinding(definition)
	if err != nil {
		return nil, err
	}
	environment := slices.Clone(runtime.Environment)
	overrides := map[string]string{"SUBYARD_INTEGRATION_SOURCE_BINDING": binding, "SUBYARD_INTEGRATION_SELECTED": "0", "INCUS_PROJECT": runtime.Yard.IncusProject, "YARD_INSTANCE_NAME": runtime.Yard.YardInstanceName, "YARD_KIND": string(runtime.Yard.YardKind), "DEV_USER": runtime.devUser(), "DEV_UID": runtime.environmentDefault("DEV_UID", "1000"), "SUBYARD_REPOSITORY_ROOT": runtime.RepositoryRoot}
	// Supply the validated exact native directory. The handler cannot choose a
	// different sidecar after package binding was assessed.
	if len(definition.Native) != 0 {
		path, err := definition.NativeExecutablePath(definition.Native[0].Path)
		if err != nil {
			return nil, err
		}
		root := path
		for range strings.Split(definition.Native[0].Path, string(filepath.Separator)) {
			root = filepath.Dir(root)
		}
		for _, native := range definition.Native {
			resolved, err := definition.NativeExecutablePath(native.Path)
			if err != nil || resolved != filepath.Join(root, native.Path) {
				return nil, errors.New("integration native artifacts have inconsistent roots")
			}
		}
		overrides["SUBYARD_INTEGRATION_NATIVE_ROOT"] = root
	}
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
	return runtime.observeIntegrationLifecyclePhase(ctx, definition, "inspect")
}
func (runtime Runtime) observeIntegrationLifecyclePhase(ctx context.Context, definition profile.IntegrationDefinition, phase string) (ports.RuntimeObservation, error) {
	environment, err := runtime.integrationLifecycleEnvironment(definition)
	if err != nil {
		return ports.RuntimeObservation{}, runtime.integrationDiagnostic(err, definition, phase, "native-unavailable")
	}
	stdout, stderr, err := runtime.runShippedHandler(ctx, profile.Definition{Root: definition.Root}, definition.Lifecycle.Handler, environment, profileRuntimeObserveLimit, "observe")
	if err != nil {
		_, reason := lifecycleFailure(stderr, "inspect")
		return ports.RuntimeObservation{}, runtime.integrationDiagnostic(err, definition, phase, reason)
	}
	observation, err := decodeProfileRuntimeObservation(stdout)
	selected := slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), definition.Name)
	if err == nil && (observation.State == ports.RuntimeStateDeferred || observation.State == ports.RuntimeStateAbsent && selected || observation.HookBinding != "" || observation.State == ports.RuntimeStateStale && (observation.Actual == "" || observation.Actual == observation.Desired)) {
		err = errors.New("invalid integration lifecycle state")
	}
	return observation, runtime.integrationDiagnostic(err, definition, phase, "invalid-schema")
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
	sources, packageSources, err := runtime.integrationSourceBindingsPhase("apply")
	if err != nil {
		return err
	}
	if sources != plan.sources {
		return runtime.integrationPlanDiagnostic(domain.ErrPlanStale, plan, IntegrationPlan{packageSources: packageSources, facts: plan.facts})
	}
	definitions, err := runtime.integrationDefinitions()
	if err != nil {
		return err
	}
	for _, definition := range definitions {
		if definition.Lifecycle == nil {
			continue
		}
		observation, err := runtime.observeIntegrationLifecyclePhase(ctx, definition, "apply")
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
			return runtime.integrationDiagnostic(domain.ErrPlanStale, definition, "apply", "observation-changed")
		}
		environment, err := runtime.integrationLifecycleEnvironment(definition)
		if err != nil {
			return runtime.integrationDiagnostic(err, definition, "apply", "native-unavailable")
		}
		operationID := runtime.environmentValue("SUBYARD_OPERATION_ID")
		if !domain.SafeID(operationID) {
			return errors.New("integration lifecycle requires a valid operation ID")
		}
		stdout, stderr, err := runtime.runShippedHandler(ctx, profile.Definition{Root: definition.Root}, definition.Lifecycle.Handler, environment, integrationLifecycleApplyLimit, "apply", operationID, observation.Actual, observation.Desired)
		if err != nil {
			phase, reason := lifecycleFailure(stderr, "apply")
			return runtime.integrationDiagnostic(err, definition, phase, reason)
		}
		applied, err := decodeProfileRuntimeObservation(stdout)
		if err != nil {
			return runtime.integrationDiagnostic(err, definition, "verify", "invalid-schema")
		}
		selected := slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), definition.Name)
		if !(applied.State == ports.RuntimeStateAbsent && !selected) && (applied.State != ports.RuntimeStateCurrent || applied.Actual != observation.Desired || applied.Desired != observation.Desired) {
			return runtime.integrationDiagnostic(errors.New("integration lifecycle apply did not converge"), definition, "verify", "convergence-failed")
		}
		verified, err := runtime.observeIntegrationLifecyclePhase(ctx, definition, "verify")
		if err != nil {
			return err
		}
		if verified != applied {
			return runtime.integrationDiagnostic(errors.New("integration lifecycle independent verification failed"), definition, "verify", "convergence-failed")
		}
	}
	return nil
}
