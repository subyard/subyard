package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/resource"
)

func stepState(converged, conditional bool) (domain.StepDecision, string) {
	if converged {
		return domain.StepSkip, "converged"
	}
	if conditional {
		return domain.StepConditional, "unknown"
	}
	return domain.StepApply, "not converged"
}

func (execution *initExecution) operationSteps() []domain.OperationStep {
	yard := execution.loaded.Context
	target := yard.IncusProject + "/" + yard.YardInstanceName
	steps := []domain.OperationStep{}
	appendStep := func(id, scope, desired, verify, consequence string, converged, conditional bool) {
		decision, observed := stepState(converged, conditional)
		step := domain.OperationStep{ID: id, Target: scope, Observed: observed, Desired: desired, Decision: decision,
			Preconditions: []string{"owner configuration and captured source identities are unchanged", "native ownership and permission guards pass"}, Verify: verify, Consequence: consequence}
		if len(steps) != 0 {
			step.DependsOn = []string{steps[len(steps)-1].ID}
		}
		steps = append(steps, step)
	}
	if execution.mode == initReconcile {
		for _, native := range execution.plan.Steps {
			if native.Stage.ID == ports.ReconcileStageIncus {
				pool := execution.loaded.Environment["SRV_POOL"]
				if pool == "" {
					pool = "default"
				}
				desired := fmt.Sprintf("%s; storage pool %s; operator UID %d", native.Stage.Label, pool, os.Getuid())
				if os.Getuid() != 0 {
					desired += "; install ACL tools if missing for native socket and policy lock access"
				}
				appendStep("init.stage.incus", "owner default Incus server", desired,
					"native VerifyStage incus confirms operator access, server API, storage and bridge", desired, native.Converged, native.Conditional)
				break
			}
		}
	}
	if execution.orphanIngressDeferred {
		appendStep("init.ingress.inspect", target, "no previously unapproved deselected public ingress",
			"inspect declared resource routes after owner Incus access is restored; refuse discovered cleanup", "restore owner Incus access and inspect deselected public ingress", false, true)
	}
	if execution.orphanIngress != nil {
		for _, entry := range execution.orphanIngress.entries {
			appendStep("init.ingress."+entry.definition.Name, target+":"+entry.contract.Device, "owned public ingress absent; preserve guest state",
				"inspect local device, ownership marker and native shutdown postcondition", "close owned public ingress for "+entry.definition.Command, false, false)
		}
	}
	if execution.integrationSelection != nil {
		appendStep("init.selection", execution.integrationSelection.path, "requested integrations ["+strings.Join(execution.loaded.Integrations.Requested, ", ")+"]",
			"read exact canonical integration settings", "record the selected yard's requested integration set", false, false)
	}
	if execution.bootstrap != nil {
		appendStep("init.registration", execution.bootstrap.targetPath, "create protected yard definition from profile "+execution.bootstrap.profile,
			"read exact published registration bytes and validate protected mode", "create named yard definition from profile "+execution.bootstrap.profile, false, false)
	}
	appendStep("init.host-id", yard.Paths.ConfigHome+"/host-id", "owner HostID "+execution.hostID,
		"read protected HostID and compare to the approved identity", "record owner HostID "+execution.hostID, !execution.hostIDPending, false)
	earlyKeys := false
	if execution.profileSetup != nil {
		for _, setup := range execution.profileSetup.items {
			if setup.key != nil && !earlyKeys {
				for _, native := range execution.plan.Steps {
					if native.Stage.ID == ports.ReconcileStageKeys {
						appendStep("init.stage.keys", target, native.Stage.Label,
							"native VerifyStage keys returns converged", native.Stage.Label, native.Converged, native.Conditional)
						earlyKeys = true
						break
					}
				}
			}
			consequences := (&initProfileSet{items: []*initProfileSetup{setup}}).consequences()
			appendStep("init.profile."+setup.definition.Name, setup.path, "protected profile settings and native consumer credential for "+setup.definition.Name,
				"read protected profile settings and validate native consumer mapping", strings.Join(consequences, "; "), false, false)
		}
	}
	if execution.mode == initReset {
		if execution.resetBaseline != nil {
			for _, step := range execution.resetBaseline.steps() {
				step.ID = "init.reset." + step.ID
				for index := range step.DependsOn {
					step.DependsOn[index] = "init.reset." + step.DependsOn[index]
				}
				steps = append(steps, step)
			}
		} else {
			for index, resource := range execution.teardownResources {
				appendStep(fmt.Sprintf("init.reset.resource.%d", index), target+":"+resource.Kind+"/"+resource.Pool+"/"+resource.Name,
					"absent before reconstruction", "native identity guard and absence check", "delete approved "+resource.Kind+" "+resource.Name, false, false)
			}
		}
		appendStep("init.reset", target, "approved instance and disk data absent before reconstruction", "inspect approved teardown absence", "delete the yard instance and its disk data", false, false)
	}
	if execution.mode == initConfigs {
		appendStep("init.configs", target, "current agent instructions and default configs", "native ConfigsConverged returns true", "refresh in-yard agent instructions and default configs", !execution.configsChanged, false)
		return steps
	}
	for _, native := range execution.plan.Steps {
		if execution.mode == initReconcile && native.Stage.ID == ports.ReconcileStageIncus || earlyKeys && native.Stage.ID == ports.ReconcileStageKeys {
			continue
		}
		scope := target
		if native.Stage.ID == ports.ReconcileStageNetworkPolicy || native.Stage.ID == ports.ReconcileStagePowerImport {
			names := make([]string, 0, len(execution.powerYards))
			for _, peer := range execution.powerYards {
				names = append(names, peer.IncusProject+"/"+peer.YardInstanceName)
			}
			scope = "registered owner yards [" + strings.Join(names, ", ") + "]"
		}
		desired := native.Stage.Label
		if native.Stage.ID == ports.ReconcileStageNetwork {
			scope = "owner host networking for " + target + " bridge " + yard.IncusBridge
			desired = fmt.Sprintf("host NetworkManager and route safety; bridge DHCP/DNS and forwarding rules if UFW is active; native policy lock access for operator UID %d", os.Getuid())
		}
		appendStep("init.stage."+string(native.Stage.ID), scope, desired,
			"native VerifyStage "+string(native.Stage.ID)+" returns converged", desired, native.Converged, native.Conditional)
	}
	steps = append(steps, execution.runtimePlan.OperationSteps(target)...)
	// Hooks and final power may become observable only after approved stages.
	// They stay in the original step set even if those stages converge meanwhile.
	if execution.hookPlan != nil {
		steps = append(steps, execution.hookPlan.OperationStep(target))
	} else {
		appendStep("init.project-hooks", target, "retry installed project hooks for the captured project set",
			"native hook runner reports each installed hook outcome", "retry installed project hooks once for active resources", execution.hooksOnly() && !execution.hooksApplicable, !execution.hooksOnly())
	}
	appendStep("init.finalize", target, "configured desired power and host boot reconciliation", "native VerifyStage finalize returns converged",
		"restore and commit the configured desired yard power state", execution.hooksOnly(), !execution.hooksOnly())
	steps = append(steps, execution.profileProvision.operationSteps(execution.loaded)...)
	return steps
}

func (execution *initExecution) stateBinding() string {
	type registration struct {
		Profile, Source, Target string
		Content                 []byte
	}
	var bootstrap *registration
	if execution.bootstrap != nil {
		bootstrap = &registration{execution.bootstrap.profile, execution.bootstrap.sourcePath, execution.bootstrap.targetPath, execution.bootstrap.content}
	}
	type selection struct {
		Path    string
		Before  config.PersistentFileSnapshot
		Desired []byte
	}
	var integration *selection
	if execution.integrationSelection != nil {
		integration = &selection{Path: execution.integrationSelection.path, Before: execution.integrationSelection.before}
		if execution.integrationSelection.write != nil {
			integration.Desired = execution.integrationSelection.write.Content
		}
	}
	type profileSetup struct {
		Name, Path, KeyPath string
		Before              config.PersistentFileSnapshot
		Values              map[string]any
		KeyBinding          string
	}
	setups := []profileSetup{}
	if execution.profileSetup != nil {
		for _, setup := range execution.profileSetup.items {
			captured := profileSetup{Name: setup.definition.Name, Path: setup.path, KeyPath: setup.keyPath, Before: setup.before, Values: setup.values}
			if setup.key != nil {
				captured.KeyBinding = setup.key.Binding
			}
			setups = append(setups, captured)
		}
	}
	return operationStateDigest(struct {
		Bootstrap *registration
		Selection *selection
		Profiles  []profileSetup
		HostID    string
		Peers     []domain.Context
		Stages    any
		Teardown  any
		Reset     string
		Provision string
		Runtimes  string
		Hooks     string
	}{
		bootstrap, integration, setups, execution.hostID, slices.Clone(execution.powerYards), execution.approvedPlan, execution.teardownResources, execution.resetBinding(), execution.profileProvision.stateBinding(), execution.runtimePlan.Binding(), execution.hookPlan.Binding()})
}

func (execution *initExecution) resetBinding() string {
	if execution.resetBaseline == nil {
		return ""
	}
	return execution.resetBaseline.binding()
}

func (execution *provisionExecution) operationSteps(loaded config.Loaded) []domain.OperationStep {
	if execution == nil {
		return nil
	}
	target := loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName
	steps := []domain.OperationStep{}
	if execution.endpoint != nil {
		steps = append(steps, domain.OperationStep{ID: "provision.endpoint", Target: execution.endpoint.path, Observed: "captured protected registration",
			Desired: fmt.Sprint(execution.endpoint.values), Decision: domain.StepApply,
			Preconditions: []string{"captured source, owner interface and address remain valid"}, Verify: "read exact published endpoint settings and validate mode",
			Consequence: strings.Join(execution.endpoint.consequences(), "; ")})
	}
	decision, observed := stepState(!execution.requiresPowerCycle, false)
	steps = append(steps, domain.OperationStep{ID: "provision.power", Target: target, Observed: observed, Desired: "temporarily running; restore captured desired power",
		Decision: decision, Preconditions: []string{"native power and start safety guards pass"}, Verify: "native power restoration verified", Consequence: "temporarily start the yard if required and restore its desired power"})
	for _, profile := range execution.profiles {
		conditional := execution.requiresPowerCycle
		decision, observed := stepState(!slices.Contains(execution.changedProfiles, profile), conditional)
		steps = append(steps, domain.OperationStep{ID: "provision.profile." + profile, Target: target + ":profile " + profile,
			Observed: observed, Desired: "converged profile " + profile, Decision: decision,
			Preconditions: []string{"captured profile selection and native profile-check contract are unchanged"}, DependsOn: []string{"provision.power"},
			Verify: "native profile check returns converged", Consequence: "provision profile " + profile})
	}
	for _, definition := range execution.startupSlots {
		eligible := slices.ContainsFunc(execution.startupSeeds, func(seed resource.Definition) bool { return seed.Command == definition.Command })
		decision, observed := stepState(!eligible, execution.startupSeedDeferred)
		steps = append(steps, domain.OperationStep{ID: "provision.startup." + definition.Name, Target: target + ":" + startupIntentKey(definition),
			Observed: observed, Desired: "pending first-start resource activation", Decision: decision,
			Preconditions: []string{"native startup seed ownership is clean; no public ingress is opened"}, DependsOn: []string{"provision.profile." + definition.Profile},
			Verify: "read native startup intent pending or already enabled", Consequence: "arm first-start activation of " + definition.Command})
	}
	return steps
}

func (execution *provisionExecution) stateBinding() string {
	if execution == nil {
		return ""
	}
	type endpoint struct {
		Path      string
		Before    config.PersistentFileSnapshot
		Content   []byte
		Values    map[string]string
		Contracts any
	}
	var saved *endpoint
	if execution.endpoint != nil {
		saved = &endpoint{execution.endpoint.path, execution.endpoint.before, execution.endpoint.content, execution.endpoint.values, execution.endpoint.contracts}
	}
	return operationStateDigest(struct {
		Profiles any
		Power    bool
		Endpoint *endpoint
		Seeds    any
	}{execution.approvedProfiles, execution.approvedPowerCycle, saved, execution.startupSeeds})
}
