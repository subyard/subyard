package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
)

// Command admission is the single owner of per-invocation command policy: whether an
// invocation only reads, how it passes the release mutation gate, which early dispatch
// precedes the gate and which bounded recovery may replace a blocked gate. The manifest
// supplies the command-level effect; handlerAdmissions refines mixed commands by verb.

// gatePolicy is how an invocation passes the release mutation gate before dispatch.
type gatePolicy int

const (
	// gateRequired consults the gate before dispatch.
	gateRequired gatePolicy = iota
	// gateSkipRead admits readers without consulting the gate.
	gateSkipRead
	// gateSkipRegistrationRepair admits config repair-registration, which owns its guards.
	gateSkipRegistrationRepair
	// gateHandlerOwned admits update, which inspects and recovers the release itself.
	gateHandlerOwned
	// gateAfterScope defers the gate to teardown scope selection; full uninstall
	// uses its own ownership and cleanup guards without requiring migrations.
	gateAfterScope
)

// earlyPath is a dispatch that completes before the release gate.
type earlyPath int

const (
	earlyNone earlyPath = iota
	// Before the selected yard is published to the environment.
	earlyNetworkHelp
	earlyProfileCompletion
	// After the selected yard is published to the environment.
	earlyCurrentMigration
	earlyHostRename // handled only when the host command owns the rename
	earlyConfigRepairAlias
	earlyConfigReduceSelection
	earlyConfigValidate
	earlyHelp
	earlyRPC
	earlyMigrate
	earlyReleaseTransition
	earlyHostLogs
)

// invocationAdmission is the classified policy of one parsed invocation.
type invocationAdmission struct {
	handler            string // core handler; empty for profile resources
	readOnly           bool
	registrationRepair bool
	gate               gatePolicy
	// Fallback repairs tried, in order, when the gate remains blocked.
	completedScalarAuthoring  bool
	activationSelectionRepair bool
	early                     earlyPath
	earlyArguments            []string
	// teardownScope selects the teardown scope after context loading.
	teardownScope bool
	// Owner inventory transaction and local HostID rename recovery before dispatch.
	ownerRecovery bool
	// Remote canonical selectors resolve through read-only owner inventory routes.
	readOnlyRoute bool
	configSync    bool
	syncCheck     bool
	syncStatus    bool
	// stop is yard stop, the one exit admitted over an unfinished activation.
	stop bool
}

// handlerAdmission refines manifest policy for one core handler.
type handlerAdmission struct {
	// readOnly reports verb-level readers of a command whose manifest effect is mutate.
	readOnly func(command.Definition, []string) bool
	// readOnlyOnly makes readOnly the whole classification, ignoring help and manifest effect.
	readOnlyOnly bool
	// ownsRecovery skips generic owner recovery; the handler performs its own.
	ownsRecovery bool
	// readOnlyRoute resolves remote canonical selectors without owner writes.
	readOnlyRoute bool
}

var handlerAdmissions = map[string]handlerAdmission{
	"@client-projects": {readOnly: func(command.Definition, []string) bool { return true }},
	"@host":            {readOnly: argumentsOnly(hostReadOnlyInvocation)},
	"@keys":            {readOnly: keysReadOnlyInvocation},
	"@config": {
		readOnly: func(_ command.Definition, arguments []string) bool {
			_, check, status := configSyncInvocation(arguments)
			return configReadOnlyInvocation(arguments) || check || status
		},
	},
	"@test-vms": {readOnly: argumentsOnly(func(arguments []string) bool {
		return testVMStatusInvocation(arguments) || testVMLogsInvocation(arguments)
	})},
	"@integration": {
		readOnly:     argumentsOnly(integrationReadOnlyInvocation),
		ownsRecovery: true, readOnlyRoute: true,
	},
	"@profile": {
		readOnly:     argumentsOnly(profileReadOnlyInvocation),
		ownsRecovery: true, readOnlyRoute: true,
	},
	"@project-state": {readOnly: argumentsOnly(func(arguments []string) bool {
		return len(arguments) > 0 && (arguments[0] == "check-role" || arguments[0] == "preview")
	})},
	"@network": {readOnly: argumentsOnly(func(arguments []string) bool {
		return len(arguments) > 0 && arguments[0] == "status"
	})},
	"@update": {readOnly: argumentsOnly(func(arguments []string) bool {
		return slices.Contains(arguments, "--check")
	})},
	// Revocation must remain available during release recovery. Treat it like a
	// reader only for preflight; its own typed action still audits the bounded
	// mutation and never grants access.
	"@ssh-agent": {readOnlyOnly: true, readOnly: argumentsOnly(func(arguments []string) bool {
		invocation, err := parseSSHAgentArguments(arguments)
		return err != nil || invocation.verb != "unlock" || invocation.help
	})},
	"@teardown": {ownsRecovery: true, readOnlyRoute: true},
	"@cleanup":  {ownsRecovery: true, readOnlyRoute: true},
	"@project":  {readOnlyRoute: true},
}

func argumentsOnly(predicate func([]string) bool) func(command.Definition, []string) bool {
	return func(_ command.Definition, arguments []string) bool { return predicate(arguments) }
}

// Only the primary keys command has the exclusive-zone reader; internal @keys
// entrypoints select a different native action through Arg0.
func keysReadOnlyInvocation(definition command.Definition, arguments []string) bool {
	if definition.Name != "keys" || definition.Arg0 != "" {
		return false
	}
	arguments = keysWithoutConsent(arguments)
	return len(arguments) == 2 && arguments[0] == "check-exclusive"
}

// admission classifies one invocation after its command lookup and owner service selection.
// It is pure: no observation, audit or state change happens here.
func (cli *CLI) admission(
	definition command.Definition,
	core bool,
	resourceDefinition resource.Definition,
	profileResource bool,
	arguments []string,
	explicit bool,
) invocationAdmission {
	var result invocationAdmission
	policy := handlerAdmission{}
	if core {
		result.handler = definition.Handler
		policy = handlerAdmissions[definition.Handler]
	}
	help := commandHelpRequested(arguments)
	switch {
	case core && policy.readOnlyOnly:
		result.readOnly = policy.readOnly(definition, arguments)
	case core:
		result.readOnly = help || definition.Effect == command.EffectRead ||
			(policy.readOnly != nil && policy.readOnly(definition, arguments))
	case profileResource:
		invocation, err := parseResourceInvocation(arguments)
		result.readOnly = err == nil && (invocation.help ||
			cli.resources.VerbReadOnly(resourceDefinition.Command, invocation.verb))
	}
	if core && definition.Handler == "@config" {
		result.registrationRepair = configRegistrationRepairInvocation(arguments)
		result.configSync, result.syncCheck, result.syncStatus = configSyncInvocation(arguments)
	}

	switch {
	case result.readOnly:
		result.gate = gateSkipRead
	case result.registrationRepair:
		result.gate = gateSkipRegistrationRepair
	case core && definition.Name == "update":
		result.gate = gateHandlerOwned
	case core && definition.Handler == "@cleanup" && slices.Contains(arguments, "--hard"):
		// The handler owns independent preservation/ownership admission for both
		// first recovery of a broken old migration and its durable journal resume.
		result.gate = gateHandlerOwned
	case core && definition.Handler == "@teardown":
		result.gate = gateAfterScope
	default:
		result.gate = gateRequired
	}
	if result.gate == gateRequired && core {
		result.completedScalarAuthoring = definition.Handler == "@config"
		result.activationSelectionRepair = true
	}

	result.stop = core && definition.Handler == "@lifecycle" && definition.Name == "stop" && !help
	result.teardownScope = core && definition.Handler == "@teardown" && !help
	result.ownerRecovery = !result.readOnly && !result.registrationRepair && !(core && policy.ownsRecovery)
	result.readOnlyRoute = result.readOnly || result.registrationRepair || (core && policy.readOnlyRoute)
	if core {
		result.early, result.earlyArguments = earlyDispatch(definition, arguments, explicit, help)
	}
	return result
}

func earlyDispatch(definition command.Definition, arguments []string, explicit, help bool) (earlyPath, []string) {
	switch definition.Handler {
	case "@network":
		if help {
			return earlyNetworkHelp, nil
		}
	case "@list":
		// Profile completion needs only shipped native inventory, before owner,
		// network, power or selected-yard observations.
		if len(arguments) == 1 && arguments[0] == "--complete-profiles" {
			return earlyProfileCompletion, nil
		}
	case "@current-migration":
		return earlyCurrentMigration, nil
	case "@host":
		return earlyHostRename, nil
	case "@config":
		switch action, rest := configEarlyInvocation(arguments); action {
		case "repair-alias":
			return earlyConfigRepairAlias, rest
		case "reduce-selection":
			return earlyConfigReduceSelection, rest
		case "validate":
			return earlyConfigValidate, rest
		}
	case "@help":
		return earlyHelp, nil
	case "@rpc":
		return earlyRPC, nil
	case "@migrate":
		return earlyMigrate, nil
	case "@release-transition":
		return earlyReleaseTransition, nil
	case "@logs":
		if !explicit && hostLogInvocation(arguments) {
			return earlyHostLogs, nil
		}
	}
	return earlyNone, nil
}

// recoveryPermits are the release-recovery grants of one invocation. The gate returns
// them as a value; Run installs them once as the invocation's permits, which prepared
// handlers read through their CLI. RPC operations and release-transition children copy
// the CLI and clear the grants that do not apply to them.
type recoveryPermits struct {
	// configApply is installed by a bounded configuration repair inside its
	// handler (config sync consumers, completed scalar authoring), never by admission.
	configApply *configApplyRepairPermit
}

// admitReleaseGate consults the release mutation gate for an invocation that requires it.
// A blocked gate may be replaced by the invocation's bounded recovery permit, or completed
// by a fallback repair. When done is true, Run returns code without dispatch.
func (cli *CLI) admitReleaseGate(
	ctx context.Context,
	yard string,
	admission invocationAdmission,
	resourceDefinition resource.Definition,
	arguments []string,
	yes bool,
) (permits recoveryPermits, code int, done bool) {
	history, historyErr := cli.readReleaseHistory()
	outcome, gateInspection, gateErr := cli.inspectCommandMutationGate(ctx, yard, admission.handler, arguments)
	if gateErr == nil {
		gateErr = historyErr
	}
	if gateErr != nil {
		cli.errorf("inspect release transition: %v", gateErr)
		return permits, 1, true
	}
	if outcome == nil {
		if err := cli.admitHistory(history, true); err != nil {
			cli.errorf("%v", err)
			return permits, 1, true
		}
		return permits, 0, false
	}
	request := gateRequest{yard: yard, handler: admission.handler, arguments: arguments, definition: resourceDefinition, profileResource: admission.handler == "", stop: admission.stop}
	if admitted, gate, scoped := cli.decideGate(ctx, request, outcome, gateInspection); admitted != nil {
		if err := cli.admitHistory(history, false); err != nil {
			cli.errorf("%v", err)
			return permits, 1, true
		}
		cli.gateAdmitted = admitted
		return permits, 0, false
	} else if scoped {
		cli.writeYardGateRefusal(yard, gate)
		return permits, 1, true
	}
	if admission.completedScalarAuthoring {
		if handled, code := cli.runCompletedConfigScalarAuthoring(ctx, yard, arguments, yes, *outcome, gateInspection); handled {
			return permits, code, true
		}
	}
	if admission.activationSelectionRepair {
		if handled, code := cli.runActivationSelectionRepair(ctx, yard, admission.handler, arguments, yes, *outcome, gateInspection); handled {
			return permits, code, true
		}
	}
	cli.writeMutationGateRefusal(outcome, gateInspection)
	return permits, 1, true
}

// writeMutationGateRefusal renders a host-wide refusal as text: the release
// state, one deduplicated cause and one next step. RPC keeps the structured
// outcome; migrate --check reports every finding.
func (cli *CLI) writeMutationGateRefusal(outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) {
	fmt.Fprint(cli.options.Stderr, mutationGateRefusalText(outcome, inspection))
}

func (cli *CLI) writeYardGateRefusal(yard string, gate yardGate) {
	for _, line := range strings.Split(yardGateRefusal(yard, gate), "\n") {
		cli.errorf("%s", line)
	}
}

func mutationGateRefusalText(outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) string {
	public := publicInspectedMutationGateOutcome(outcome, inspection)
	causes := []string{}
	retry := public.Action
	for _, blocker := range public.Blockers {
		if !slices.Contains(causes, blocker.Message) {
			causes = append(causes, blocker.Message)
		}
		if retry == "" {
			retry = blocker.Retry
		}
	}
	if len(causes) == 0 && public.Message != "" {
		causes = append(causes, public.Message)
	}
	var text strings.Builder
	transaction := ""
	if public.Transaction != nil {
		transaction = ", transaction " + string(*public.Transaction)
	}
	fmt.Fprintf(&text, "%s release %s (%s): active %s, target %s%s; this command is not run\n",
		mutationGateRefusalPrefix, public.Status, public.Code, firstNonempty(string(public.Active), "unknown"), firstNonempty(string(public.Target), "unknown"), transaction)
	if len(causes) != 0 {
		cause := causes[0]
		if len(causes) > 1 {
			cause += fmt.Sprintf(" (and %d more; yard migrate --check)", len(causes)-1)
		}
		fmt.Fprintf(&text, "yard: cause: %s\n", cause)
	}
	if retry != "" {
		fmt.Fprintf(&text, "yard: next: %s\n", retry)
	}
	return text.String()
}

// mutationGateRefusalPrefix opens every gate refusal on stderr.
const mutationGateRefusalPrefix = "yard: release gate:"

// admitTeardownGate consults the deferred teardown gate after scope selection.
func (cli *CLI) admitTeardownGate(
	ctx context.Context,
	yard string,
	arguments []string,
) (int, bool) {
	history, historyErr := cli.readReleaseHistory()
	outcome, gateInspection, gateErr := cli.inspectMutationGateInspection(ctx, yard)
	if gateErr == nil {
		gateErr = historyErr
	}
	if gateErr != nil {
		cli.errorf("inspect release transition: %v", gateErr)
		return 1, true
	}
	if outcome == nil {
		if err := cli.admitHistory(history, true); err != nil {
			cli.errorf("%v", err)
			return 1, true
		}
		return 0, false
	}
	request := gateRequest{yard: yard, handler: "@teardown", arguments: arguments}
	if admitted, gate, scoped := cli.decideGate(ctx, request, outcome, gateInspection); admitted != nil {
		if err := cli.admitHistory(history, false); err != nil {
			cli.errorf("%v", err)
			return 1, true
		}
		cli.gateAdmitted = admitted
		return 0, false
	} else if scoped {
		cli.writeYardGateRefusal(yard, gate)
		return 1, true
	}
	cli.writeMutationGateRefusal(outcome, gateInspection)
	return 1, true
}
