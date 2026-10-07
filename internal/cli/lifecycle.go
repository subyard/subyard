package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/shelladapter"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

type lifecycleExecution struct {
	action        string
	force         bool
	changed       bool
	observed      *ports.InstanceInfo
	status        string
	vmCPUReady    bool
	approvedSteps []domain.OperationStep
}

func prepareLifecycleExecution(
	definition command.Definition,
	arguments []string,
) (*lifecycleExecution, error) {
	if definition.Handler != "@lifecycle" ||
		(definition.Name != "start" && definition.Name != "stop") {
		return nil, errors.New("invalid lifecycle command")
	}
	execution := &lifecycleExecution{action: definition.Name}
	for _, argument := range arguments {
		switch argument {
		case "-y", "--yes":
		case "--force":
			if definition.Name != "stop" {
				return nil, errors.New("--force is only valid with stop")
			}
			execution.force = true
		case "-h", "--help":
			return nil, errors.New("help is not an executable lifecycle operation")
		default:
			return nil, fmt.Errorf("unknown option %q", argument)
		}
	}
	return execution, nil
}

func (execution *lifecycleExecution) policy(
	definition command.Definition,
	yard domain.Context,
) domain.CommandPolicy {
	consequences := []string{
		fmt.Sprintf("%s Incus instance %s in project %s", execution.action,
			yard.YardInstanceName, yard.IncusProject),
	}
	if execution.action == "start" {
		consequences = append(consequences,
			"verify the host route before and after the start",
			"record desired power as running only after the safety checks pass",
		)
	} else {
		if execution.force {
			consequences = append(consequences, "bypass the active SSH session guard")
		} else {
			consequences = append(consequences, "refuse to stop while an SSH session is active")
		}
		consequences = append(consequences,
			"record desired power as stopped only after the instance stops",
		)
	}
	return domain.CommandPolicy{
		Name: definition.Name, Effect: domain.CommandEffect(definition.Effect),
		RemotePolicy: domain.RemotePolicy(definition.Remote), Consequences: consequences,
	}
}

func (execution *lifecycleExecution) actionPlan(
	definition command.Definition,
	yard domain.Context,
) (domain.ActionID, domain.ActionDelta, error) {
	if execution == nil || execution.action != "stop" {
		return "", domain.ActionDelta{}, errors.New("stop execution is required")
	}
	action := domain.ActionID("yard.stop")
	if execution.force {
		action = "yard.stop-force"
	}
	delta := domain.ActionDelta{Changed: execution.changed}
	if delta.Changed {
		delta.Consequences = execution.policy(definition, yard).Consequences
	}
	return action, delta, nil
}

func (cli *CLI) observeLifecycleExecution(
	ctx context.Context,
	yard domain.Context,
	execution *lifecycleExecution,
) error {
	if execution == nil {
		return nil
	}
	incusPort, _ := cli.statusPorts()
	instance, err := incusPort.Instance(ctx, yard.IncusProject, yard.YardInstanceName)
	if err != nil {
		return err
	}
	metadata := instance
	metadata.Status = ""
	if execution.observed != nil {
		approved := *execution.observed
		approved.Status = ""
		if operationStateDigest(approved) != operationStateDigest(metadata) {
			return fmt.Errorf("%w: lifecycle target metadata changed", domain.ErrPlanStale)
		}
		if execution.action == "stop" && strings.EqualFold(execution.status, "stopped") && !strings.EqualFold(instance.Status, "stopped") {
			return fmt.Errorf("%w: stopped lifecycle target requires new work", domain.ErrPlanStale)
		}
	} else {
		payload, _ := json.Marshal(instance)
		var copy ports.InstanceInfo
		_ = json.Unmarshal(payload, &copy)
		execution.observed = &copy
	}
	desired := "stopped"
	if execution.action == "start" {
		desired = "running"
	}
	execution.changed = !strings.EqualFold(instance.Status, desired)
	execution.status = instance.Status
	if execution.action == "start" && !execution.changed && execution.observed.Config["user.subyard.vm_cpu_weight"] != "" {
		scheduler, ok := incusPort.(interface {
			VMCPUConverged(context.Context, string, string, bool) (bool, error)
		})
		if !ok {
			return errors.New("native VM host CPU scheduling is unavailable")
		}
		execution.vmCPUReady, err = scheduler.VMCPUConverged(ctx, yard.IncusProject, yard.YardInstanceName, false)
		if err != nil {
			return err
		}
	}
	steps := execution.steps(yard)
	if execution.approvedSteps != nil {
		return domain.CheckOperationSteps(execution.approvedSteps, steps)
	}
	execution.approvedSteps = domain.CloneOperationSteps(steps)
	return nil
}

func (cli *CLI) executeLifecycle(
	ctx context.Context,
	orchestrator *application.Orchestrator,
	yard domain.Context,
	plan domain.OperationPlan,
	execution *lifecycleExecution,
	diagnostics io.Writer,
) (domain.AdapterResult, error) {
	if execution == nil {
		return domain.AdapterResult{}, errors.New("lifecycle execution is required")
	}
	if execution.observed == nil {
		return domain.AdapterResult{}, errors.New("captured lifecycle target is required")
	}
	if err := cli.observeLifecycleExecution(ctx, yard, execution); err != nil {
		return domain.AdapterResult{}, err
	}
	if execution.action == "stop" && !execution.changed {
		return domain.AdapterResult{Schema: 1, OperationID: plan.OperationID, Status: "ok"}, nil
	}
	if execution.action == "start" && cli.options.AdapterRunner == nil {
		if err := cli.prepareNetworkManagerPrivileges(
			ctx, diagnostics, cli.effectiveUID(), execution.action,
		); err != nil {
			return domain.AdapterResult{}, err
		}
		if execution.observed.Config["user.subyard.vm_cpu_weight"] != "" {
			if err := cli.prepareLifecycleVMCPUPrivileges(ctx, diagnostics, yard, execution); err != nil {
				return domain.AdapterResult{}, err
			}
		}
	}
	power, err := cli.lifecyclePowerService()
	if err != nil {
		return domain.AdapterResult{}, err
	}
	contextValues := structuredAdapterContext(yard)
	if weight := execution.observed.Config["user.subyard.vm_cpu_weight"]; weight != "" {
		contextValues["VM_CPU_WEIGHT"] = weight
		contextValues["SUBYARD_DISPATCHER_PATH"] = cli.options.DispatcherPath
	}
	arguments := make([]string, 0, 1)
	if execution.force {
		arguments = append(arguments, "--force")
	}
	if execution.action == "start" && cli.env["SUBYARD_SUDO_PREAUTHORIZED"] == "1" {
		contextValues["SUBYARD_SUDO_PREAUTHORIZED"] = "1"
	}
	orchestrator.Runner = application.LifecycleRunner{
		Power:    power,
		Physical: orchestrator.Runner, Yard: yard,
	}
	request := domain.AdapterRequest{
		Schema: shelladapter.ProtocolSchema, OperationID: plan.OperationID,
		Adapter: "lifecycle", Action: execution.action, Arguments: arguments, Context: contextValues,
	}
	var result domain.AdapterResult
	var stderr string
	run := func() error {
		var runErr error
		result, stderr, runErr = orchestrator.RunAdapter(ctx, plan, request, nil)
		return runErr
	}
	if execution.action == "start" {
		service := cli.networkService([]domain.Context{yard})
		if service == nil {
			return domain.AdapterResult{}, errors.New("Incus network policy adapter is unavailable")
		}
		err = service.WithStart(ctx, networkYard(yard), run)
	} else {
		err = run()
	}
	writeAdapterDiagnostics(diagnostics, stderr)
	if err == nil && result.Status == "ok" {
		if execution.action == "start" {
			fmt.Fprintf(diagnostics, "  [ ok ] %s started (desired=running)\n", yard.YardInstanceName)
		} else {
			fmt.Fprintf(diagnostics, "  [ ok ] %s stopped (desired=stopped)\n", yard.YardInstanceName)
		}
	}
	return result, err
}

func (cli *CLI) prepareLifecycleVMCPUPrivileges(ctx context.Context, diagnostics io.Writer, yard domain.Context, execution *lifecycleExecution) error {
	if !execution.changed {
		incusPort, _ := cli.statusPorts()
		scheduler, ok := incusPort.(interface {
			VMCPUConverged(context.Context, string, string, bool) (bool, error)
		})
		if !ok {
			return errors.New("native VM host CPU scheduling is unavailable")
		}
		ready, err := scheduler.VMCPUConverged(ctx, yard.IncusProject, yard.YardInstanceName, false)
		if err != nil {
			return err
		}
		execution.vmCPUReady = ready
		if execution.approvedSteps != nil {
			if err := domain.CheckOperationSteps(execution.approvedSteps, execution.steps(yard)); err != nil {
				return err
			}
		}
		if ready {
			return nil
		}
	}
	return cli.prepareSudoPrivileges(ctx, diagnostics, cli.effectiveUID(), execution.action)
}

func (cli *CLI) lifecyclePowerService() (application.PowerService, error) {
	incusPort, _ := cli.statusPorts()
	configWriter, ok := incusPort.(ports.InstanceConfigWriter)
	if !ok {
		return application.PowerService{}, errors.New("Incus instance config writer is required")
	}
	return application.PowerService{Instances: incusPort, Config: configWriter}, nil
}

func (cli *CLI) preparePowerIntent(ctx context.Context, yard domain.Context) (string, error) {
	power, err := cli.lifecyclePowerService()
	if err != nil {
		return "", err
	}
	intent, err := power.Ensure(ctx, yard)
	if err != nil {
		return "", err
	}
	return intent.Desired, nil
}

func (execution *lifecycleExecution) binding() string {
	return operationStateDigest(struct {
		Action   string
		Force    bool
		Instance *ports.InstanceInfo
	}{execution.action, execution.force, execution.observed})
}
func (execution *lifecycleExecution) steps(yard domain.Context) []domain.OperationStep {
	if execution == nil || execution.observed == nil {
		return nil
	}
	desired := "stopped"
	if execution.action == "start" {
		desired = "running"
	}
	decision := domain.StepApply
	if !execution.changed {
		decision = domain.StepSkip
	}
	target := "incus/" + yard.IncusProject + "/instance/" + yard.YardInstanceName
	steps := []domain.OperationStep{{ID: "power", Target: target, Observed: strings.ToLower(execution.status), Desired: desired, Decision: decision, Preconditions: []string{"captured instance configuration and ownership remain unchanged"}, Verify: "instance " + desired + " before desired power commit", Consequence: execution.action + " yard instance"}}
	if weight := execution.observed.Config["user.subyard.vm_cpu_weight"]; execution.action == "start" && weight != "" {
		observed, decision := "drift", domain.StepApply
		if execution.changed {
			observed, decision = strings.ToLower(execution.status), domain.StepConditional
		} else if execution.vmCPUReady {
			observed, decision = "current", domain.StepSkip
		}
		steps = append(steps, domain.OperationStep{ID: "vm-cpu", Target: target, Observed: observed,
			Desired: "cpu.weight=" + weight, Decision: decision,
			Preconditions: []string{"captured instance configuration and ownership remain unchanged"}, DependsOn: []string{"power"},
			Verify: "host CPU scheduling matches persisted weight", Consequence: "converge yard host CPU scheduling"})
	}
	return steps
}
