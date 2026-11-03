package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

const (
	// Integration observations can include a bounded service startup wait.
	// This aggregate cap includes all probes, transport and projection; their
	// individual limits cannot all consume their full allowance. Startup gets
	// margin without extending profile observers or cancellation ownership.
	integrationLifecycleObserveLimit = 2 * time.Minute
	// Integration install includes the existing bounded image pull and package work.
	integrationLifecycleApplyLimit = 22 * time.Minute
)

func (runtime Runtime) integrationDefinitions() ([]profile.IntegrationDefinition, error) {
	if runtime.Integrations != nil {
		return slices.Clone(runtime.Integrations), nil
	}
	return profile.LoadIntegrations(runtime.RepositoryRoot)
}

func (runtime Runtime) integrationPackageBindingWithSourceReader(definition profile.IntegrationDefinition, reader func(config.MaterializedAsset) ([]byte, error)) (string, error) {
	source, err := definition.SourceBinding()
	if err != nil {
		return "", err
	}
	values := map[string]string{}
	for _, setting := range definition.Settings {
		values[setting.Name] = runtime.bindingPath(runtime.environmentValue(setting.Name))
	}
	for _, input := range definition.Inputs {
		values[input.Name] = runtime.bindingPath(input.Value)
	}
	if path := runtime.environmentValue("AGENT_" + definition.Name + "_PROVISION"); path != "" {
		var data []byte
		var err error
		if reader == nil {
			data, err = (config.MaterializedAsset{Source: path}).ReadSource()
		} else {
			data, err = reader(config.MaterializedAsset{Source: path})
		}
		if err != nil {
			return "", err
		}
		values["AGENT_"+definition.Name+"_PROVISION"] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	payload, _ := json.Marshal(struct {
		Source string
		Values map[string]string
	}{source, values})
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}
func (runtime Runtime) integrationSourceBindingsPhase(phase string) (string, map[string]string, error) {
	return runtime.integrationSourceBindingsWithSourceReader(phase, runtime.SourceReader)
}

func (runtime Runtime) integrationSourceBindingsWithSourceReader(phase string, reader func(config.MaterializedAsset) ([]byte, error)) (string, map[string]string, error) {
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
		binding, err := runtime.integrationPackageBindingWithSourceReader(definition, reader)
		if err != nil {
			return "", nil, runtime.integrationDiagnostic(err, definition, phase, "native-unavailable")
		}
		sources[definition.Name] = binding
	}
	payload, _ := json.Marshal(sources)
	return fmt.Sprintf("%x", sha256.Sum256(payload)), sources, nil
}
func (runtime Runtime) integrationLifecycleEnvironment(definition profile.IntegrationDefinition) ([]string, error) {
	return runtime.integrationLifecycleEnvironmentWithSourceReader(definition, runtime.SourceReader)
}
func (runtime Runtime) integrationLifecycleEnvironmentWithSourceReader(definition profile.IntegrationDefinition, reader func(config.MaterializedAsset) ([]byte, error)) ([]string, error) {
	binding, err := runtime.integrationPackageBindingWithSourceReader(definition, reader)
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

// integrationLifecycleObservation adds an optional public notice to the shared
// runtime observation. Only retired packages may report one.
type integrationLifecycleObservation struct {
	ports.RuntimeObservation
	Notice string
}

const integrationNoticeLimit = 440

func decodeIntegrationLifecycleObservation(definition profile.IntegrationDefinition, payload []byte) (integrationLifecycleObservation, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var wire struct {
		ports.RuntimeObservation
		Notice string `json:"notice"`
	}
	if err := decoder.Decode(&wire); err != nil {
		return integrationLifecycleObservation{}, fmt.Errorf("decode lifecycle observation: %w", err)
	}
	if wire.Notice != "" && (!definition.Retired || wire.State == ports.RuntimeStateCurrent || len(wire.Notice) > integrationNoticeLimit || strings.TrimSpace(wire.Notice) != wire.Notice || strings.ContainsFunc(wire.Notice, unicode.IsControl)) {
		return integrationLifecycleObservation{}, errors.New("lifecycle observation has an invalid notice")
	}
	canonical, err := json.Marshal(wire.RuntimeObservation)
	if err != nil {
		return integrationLifecycleObservation{}, err
	}
	observation, err := decodeProfileRuntimeObservation(append(canonical, payload[decoder.InputOffset():]...))
	if err != nil {
		return integrationLifecycleObservation{}, err
	}
	return integrationLifecycleObservation{RuntimeObservation: observation, Notice: wire.Notice}, nil
}

func (runtime Runtime) observeIntegrationLifecycle(ctx context.Context, definition profile.IntegrationDefinition) (integrationLifecycleObservation, error) {
	return runtime.observeIntegrationLifecyclePhase(ctx, definition, "inspect")
}
func (runtime Runtime) observeIntegrationLifecyclePhase(ctx context.Context, definition profile.IntegrationDefinition, phase string) (integrationLifecycleObservation, error) {
	return runtime.observeIntegrationLifecycleWithSourceReader(ctx, definition, phase, runtime.SourceReader)
}
func (runtime Runtime) observeIntegrationLifecycleWithSourceReader(ctx context.Context, definition profile.IntegrationDefinition, phase string, reader func(config.MaterializedAsset) ([]byte, error)) (integrationLifecycleObservation, error) {
	environment, err := runtime.integrationLifecycleEnvironmentWithSourceReader(definition, reader)
	if err != nil {
		return integrationLifecycleObservation{}, runtime.integrationDiagnostic(err, definition, phase, "native-unavailable")
	}
	started := time.Now()
	stdout, stderr, err := runtime.runShippedHandler(ctx, profile.Definition{Root: definition.Root}, definition.Lifecycle.Handler, environment, integrationLifecycleObserveLimit, "observe")
	if err != nil {
		_, reason := lifecycleFailure(stderr, "inspect")
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason = "native-unavailable"
		}
		return integrationLifecycleObservation{}, runtime.integrationExecutionDiagnostic(err, definition, phase, "inspect", reason, stderr, time.Since(started), integrationLifecycleObserveLimit, ctx.Err())
	}
	observation, err := decodeIntegrationLifecycleObservation(definition, stdout)
	selected := slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), definition.Name)
	if err == nil && (observation.State == ports.RuntimeStateDeferred || observation.State == ports.RuntimeStateAbsent && selected || observation.HookBinding != "" || observation.State == ports.RuntimeStateStale && (observation.Actual == "" || observation.Actual == observation.Desired)) {
		err = errors.New("invalid integration lifecycle state")
	}
	return observation, runtime.integrationDiagnostic(err, definition, phase, "invalid-schema")
}
func (runtime Runtime) integrationLifecycleFact(definition profile.IntegrationDefinition, observation integrationLifecycleObservation) integrationFact {
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
		started := time.Now()
		stdout, stderr, err := runtime.runShippedHandler(ctx, profile.Definition{Root: definition.Root}, definition.Lifecycle.Handler, environment, integrationLifecycleApplyLimit, "apply", operationID, observation.Actual, observation.Desired)
		if err != nil {
			phase, reason := lifecycleFailure(stderr, "apply")
			return runtime.integrationExecutionDiagnostic(err, definition, phase, "apply", reason, stderr, time.Since(started), integrationLifecycleApplyLimit, ctx.Err())
		}
		applied, err := decodeIntegrationLifecycleObservation(definition, stdout)
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
		// Unattended updates skip the confirmation preview, so the completed
		// retirement is stated once more at this execution boundary.
		if runtime.Stderr != nil {
			for _, notice := range []string{observation.Notice, applied.Notice} {
				if notice != "" {
					fmt.Fprintf(runtime.Stderr, "Warning: yard %s: %s\n", runtime.Yard.YardName, notice)
				}
			}
		}
	}
	return nil
}
