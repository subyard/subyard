package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// A stopped guest cannot supply account authority. This prefix authorizes only
// a temporary, guarded boot and the same bounded init scope. Every actual
// account recipe is freshly verified after boot, before any account mutation.
// identityInspectionWithoutIncus reports an unreachable Incus with no pending developer identity
// rename. Before Incus is reachable no yard instance exists to inspect, and ordinary init installs
// Incus or reports its own refusal. A pending rename still fails closed on its native query.
func identityInspectionWithoutIncus(ctx context.Context, runtime reconcileruntime.Runtime) bool {
	if runtime.Incus == nil {
		return false
	}
	if _, err := runtime.Incus.Server(ctx); err == nil {
		return false
	}
	home := runtime.Yard.Paths.ConfigHome
	if _, err := os.Lstat(home); os.IsNotExist(err) {
		return true
	}
	marker, err := config.ReadPersistentFileSnapshot(home, config.DeveloperIdentityMarkerPath(home, runtime.Yard.YardName))
	return err == nil && !marker.Exists
}

func (prepared *preparedCommand) prepareStoppedIdentityInspection(ctx context.Context, bootstrap *initBootstrap) (bool, error) {
	cli := prepared.CLI
	if cli.options.InitPlatform != nil || cli.releaseTransitionChild || bootstrap != nil {
		return false, nil
	}
	runtime := cli.initPlatform(prepared.Loaded, nil).(reconcileruntime.Runtime)
	if identityInspectionWithoutIncus(ctx, runtime) {
		return false, nil
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(instance.Status, "stopped") {
		return false, nil
	}
	marker, err := config.ReadPersistentFileSnapshot(runtime.Yard.Paths.ConfigHome, config.DeveloperIdentityMarkerPath(runtime.Yard.Paths.ConfigHome, runtime.Yard.YardName))
	if err != nil {
		return false, err
	}
	request, err := parseInitArguments(prepared.Arguments)
	if err != nil {
		return false, err
	}
	if request.mode != initReconcile || request.profile != "" || request.adopt || request.continueLegacy {
		if !marker.Exists {
			// Other modes keep their own account handling, as for a running
			// yard, and must not depend on identity hints or native bindings.
			return false, nil
		}
		return false, errors.New("stopped developer identity inspection requires plain init")
	}
	source, uuid, err := runtime.DeveloperIdentityObservationBinding(ctx)
	if err != nil {
		return false, err
	}
	release, err := identityReleaseBinding(runtime.Yard.Paths.ConfigHome)
	if err != nil {
		return false, err
	}
	hint, hintBefore, err := config.ReadAppliedIdentityHint(runtime.Yard.Paths.ConfigHome, runtime.Yard.YardName)
	if err != nil {
		return false, err
	}
	if hint != nil && hint.UUID != uuid {
		return false, fmt.Errorf("%w: applied identity hint belongs to another native yard", domain.ErrPlanStale)
	}
	trusted := hint != nil && hint.Source == source && hint.Release == release
	if trusted && hint.User == runtime.Yard.DevUser && hint.UID == runtime.Yard.DevUID && !marker.Exists {
		return false, nil
	}
	peers, err := cli.powerYardContexts(prepared.Loaded)
	if err != nil {
		return false, err
	}
	inputs, err := cli.captureOwnerInputs(prepared.Loaded, "", peers)
	if err != nil {
		return false, err
	}
	store, err := openProjectStoreReadOnly(runtime.Yard.Paths.StateDir)
	if err != nil {
		return false, err
	}
	projects, err := store.List(ctx)
	if err != nil {
		return false, err
	}
	execution := &initExecution{loaded: prepared.Loaded, mode: initReconcile, powerYards: peers}
	if prepared.Loaded.Integrations.AllowsCodingTools {
		execution.integrationRetirements, execution.integrationWarnings, err = runtime.RetiredIntegrationNotices(ctx)
		if err != nil {
			return false, err
		}
	}
	prepared.initExecution = execution

	execution.runtimePlan, err = runtime.DeferredIdentityRuntimes()
	if err != nil {
		return false, err
	}
	execution.hostID, execution.hostIDPending, err = configsync.ResolveHostID(runtime.Yard.Paths.ConfigHome, prepared.Loaded.Environment)
	if err != nil {
		return false, err
	}
	execution.hookPlan, err = runtime.PrepareProjectHooks(ctx, false, true)
	if err != nil {
		return false, err
	}
	execution.hookPlan.CaptureOwnerProjects(projects)
	hookRuntime := runtime
	hookRuntime.HookPlan = execution.hookPlan
	if err := hookRuntime.PrepareProjectMetadata(ctx); err != nil {
		return false, err
	}
	for _, stage := range application.InitStages(runtime.Yard) {
		execution.plan.Steps = append(execution.plan.Steps, application.ReconcileStep{Stage: stage, Conditional: true})
	}
	target := runtime.Yard.IncusProject + "/" + runtime.Yard.YardInstanceName
	description := fmt.Sprintf("temporarily start stopped yard to inspect its protected applied developer identity; if different, rename to %s and /home/%s preserving UID %d, primary GID and retained data; quiesce captured account services and SSH before account changes; preserve desired power", runtime.Yard.DevUser, runtime.Yard.DevUser, runtime.Yard.DevUID)
	steps := identityPrerequisiteSteps(execution.operationSteps(), target, description)
	steps[0].Decision = domain.StepConditional
	steps = append([]domain.OperationStep{{ID: "init.identity-inspection-start", Target: target, Observed: "stopped native yard; applied identity requires inspection", Desired: "temporary running state with original desired power", Decision: domain.StepApply, Verify: "same native UUID, source, configuration and project scope", Consequence: description}}, steps...)
	steps[1].DependsOn = []string{"init.identity-inspection-start"}
	steps = append(steps, domain.OperationStep{ID: "init.identity-inspection-restore", Target: target, Observed: "temporary inspection startup", Desired: "captured desired physical power", Decision: domain.StepConditional, Verify: "native power equals captured desired intent", Consequence: "restore desired physical power after successful inspected init"})
	var started *ports.InstanceInfo
	check := func(ctx context.Context, stopped bool) error {
		if err := inputs.check(ctx, cli); err != nil {
			return err
		}
		current, err := store.List(ctx)
		if err != nil {
			return err
		}
		if operationStateDigest(current) != operationStateDigest(projects) {
			return domain.ErrPlanStale
		}
		currentRelease, err := identityReleaseBinding(runtime.Yard.Paths.ConfigHome)
		if err != nil {
			return err
		}
		if currentRelease != release {
			return domain.ErrPlanStale
		}
		_, currentHint, err := config.ReadAppliedIdentityHint(runtime.Yard.Paths.ConfigHome, runtime.Yard.YardName)
		if err != nil {
			return err
		}
		if config.BindPersistentSnapshot(currentHint) != config.BindPersistentSnapshot(hintBefore) {
			return domain.ErrPlanStale
		}
		currentMarker, err := config.ReadPersistentFileSnapshot(runtime.Yard.Paths.ConfigHome, config.DeveloperIdentityMarkerPath(runtime.Yard.Paths.ConfigHome, runtime.Yard.YardName))
		if err != nil {
			return err
		}
		if config.BindPersistentSnapshot(currentMarker) != config.BindPersistentSnapshot(marker) {
			return domain.ErrPlanStale
		}
		observed, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
		if err != nil {
			return err
		}
		if stopped && operationStateDigest(observed) != operationStateDigest(instance) || observed.Config["volatile.uuid"] != uuid {
			return domain.ErrPlanStale
		}
		if !stopped && (started == nil || !strings.EqualFold(observed.Status, "running") || identityInspectionInstanceBinding(observed) != identityInspectionInstanceBinding(*started)) {
			return domain.ErrPlanStale
		}
		if !stopped {
			// Guest hook facts become observable only now and can belong to an
			// interrupted rename. The continuation captures them itself, and its
			// hook scope is bound to this plan before anything is applied.
			return nil
		}
		return hookRuntime.CheckProjectHookPlan(ctx, true)
	}
	prepared.exactState = operationStateDigest([]string{inputs.binding(), operationStateDigest(instance), operationStateDigest(projects), source, release, operationStateDigest(config.BindPersistentSnapshot(hintBefore))})
	prepared.stepsComplete = true
	prepared.steps = func() []domain.OperationStep { return domain.CloneOperationSteps(steps) }
	prepared.assess = func(context.Context) (domain.ActionID, domain.ActionDelta, error) {
		return "yard.init.reconcile", domain.ActionDelta{Changed: true, Consequences: append([]string{description}, execution.consequences()...)}, nil
	}
	prepared.refresh = func(ctx context.Context) (domain.ActionID, domain.ActionDelta, error) {
		if err := check(ctx, true); err != nil {
			return "", domain.ActionDelta{}, err
		}
		return prepared.assess(ctx)
	}
	prepared.execute = func(ctx context.Context, orchestrator *application.Orchestrator, diagnostics io.Writer) (domain.AdapterResult, error) {
		unlock, err := lockIntegrationYard(ctx, prepared.Loaded)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		err = func() error {
			if err := check(ctx, true); err != nil {
				return err
			}
			if err := cli.prepareNetworkManagerPrivileges(ctx, diagnostics, cli.effectiveUID(), "start"); err != nil {
				return err
			}
			if instance.Config["user.subyard.vm_cpu_weight"] != "" {
				if err := cli.prepareLifecycleVMCPUPrivileges(ctx, diagnostics, runtime.Yard, &lifecycleExecution{action: "start", changed: true, observed: &instance}); err != nil {
					return err
				}
			}
			power, err := cli.lifecyclePowerService()
			if err != nil {
				return err
			}
			// Ensure imports legacy missing intent from the captured stopped instance,
			// before temporary startup can make it appear intentionally running.
			expected, err := startIdentityInspectionPower(ctx, power, runtime.Yard, instance, func() error { return runtime.RunMigrationPhysicalLifecycle(ctx, "start") })
			if err == nil {
				started = &expected
			}
			return err
		}()
		unlock()
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if err := check(ctx, false); err != nil {
			return domain.AdapterResult{}, err
		}
		if trusted && !marker.Exists {
			user, uid, err := runtime.VerifiedAppliedDeveloperIdentity(ctx)
			if err != nil {
				return domain.AdapterResult{}, err
			}
			if user != hint.User || uid != hint.UID {
				return domain.AdapterResult{}, fmt.Errorf("%w: stopped identity hint disagrees with guest authority", domain.ErrPlanStale)
			}
		}
		fresh := *prepared
		fresh.interactiveSetup = false
		fresh.retirementsDisclosed = true
		handled, err := fresh.prepareDeveloperIdentityInit(ctx, nil)
		if err != nil {
			return domain.AdapterResult{}, err
		}
		if !handled {
			if err := fresh.prepareOrdinaryInit(ctx, nil); err != nil {
				return domain.AdapterResult{}, err
			}
		}
		if err := identityContinuationScope(steps, fresh.steps()); err != nil {
			return domain.AdapterResult{}, err
		}
		if err := execution.hookPlan.CheckContinuationScope(fresh.continuationHookPlan()); err != nil {
			return domain.AdapterResult{}, err
		}
		result, err := fresh.execute(ctx, orchestrator, diagnostics)
		if err != nil {
			return result, err
		}
		if err := cli.publishAppliedIdentityHint(ctx, prepared.Loaded); err != nil {
			return result, err
		}
		if err := runtime.ApplyStage(ctx, ports.ReconcileStageFinalize); err != nil {
			return result, err
		}
		return result, nil
	}
	return true, nil
}

// Record only the existing power service's writes, so boot's expected native
// envelope includes legacy intent import without accepting unrelated changes.
type identityPowerWriter struct {
	ports.InstanceConfigWriter
	updates map[string]string
}

func (writer *identityPowerWriter) SetInstanceConfig(ctx context.Context, project, name string, values map[string]string) error {
	if err := writer.InstanceConfigWriter.SetInstanceConfig(ctx, project, name, values); err != nil {
		return err
	}
	for key, value := range values {
		writer.updates[key] = value
	}
	return nil
}

// An identity continuation has no init execution of its own to publish, so a
// copy of the inspection would otherwise compare the approved scope to itself.
func (prepared *preparedCommand) continuationHookPlan() *reconcileruntime.ProjectHookPlan {
	if prepared.identityHookPlan != nil {
		return prepared.identityHookPlan
	}
	return prepared.initExecution.hookPlan
}

func startIdentityInspectionPower(ctx context.Context, power application.PowerService, yard domain.Context, before ports.InstanceInfo, start func() error) (ports.InstanceInfo, error) {
	current, err := power.Instances.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if err != nil {
		return ports.InstanceInfo{}, err
	}
	if operationStateDigest(current) != operationStateDigest(before) || !strings.EqualFold(current.Status, "stopped") {
		return ports.InstanceInfo{}, domain.ErrPlanStale
	}
	expected := before
	expected.Config = maps.Clone(before.Config)
	expected.LocalConfig = maps.Clone(before.LocalConfig)
	if expected.Config == nil {
		expected.Config = map[string]string{}
	}
	if expected.LocalConfig == nil {
		expected.LocalConfig = map[string]string{}
	}
	writer := &identityPowerWriter{InstanceConfigWriter: power.Config, updates: map[string]string{}}
	power.Config = writer
	if _, err := power.Ensure(ctx, yard); err != nil {
		return ports.InstanceInfo{}, err
	}
	for key, value := range writer.updates {
		expected.Config[key] = value
		expected.LocalConfig[key] = value
	}
	if err := start(); err != nil {
		return ports.InstanceInfo{}, err
	}
	expected.Status = "Running"
	return expected, nil
}
func identityInspectionInstanceBinding(instance ports.InstanceInfo) string {
	devices := make([]string, 0, len(instance.Devices))
	for name := range instance.Devices {
		devices = append(devices, name)
	}
	instance.Config = maps.Clone(instance.Config)
	instance.LocalConfig = maps.Clone(instance.LocalConfig)
	instance.Status = ""
	reconcileruntime.NormalizeInstanceLifecycleConfig(instance.Config, devices, instance.Type == domain.YardVM)
	reconcileruntime.NormalizeInstanceLifecycleConfig(instance.LocalConfig, devices, instance.Type == domain.YardVM)
	return operationStateDigest(instance)
}
