package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
)

// The release gate is host-wide only for host causes. A completed release whose
// remaining causes are all attributed to yards, through the engine's closed
// yard-attribution grammar, gates by the selected yard alone: an implicated yard
// that is not converged admits only its repair commands, a yard with only typed
// pending findings keeps the pending-configuration contract, and every other
// yard and host command is open. Anything outside the grammar, an unfinished or
// unmatched journal, pending migrations, host-owner drift or failure, target
// collisions and ambiguous topology keep every mutation gated.

// yardGateState is what the gate knows about one implicated yard.
type yardGateState struct {
	failed, drift, pending, config bool
	// messages are the yard's distinct causes in report order.
	messages []string
	retry    string
}

// repairOnly reports a yard that admits only its repair commands.
func (state yardGateState) repairOnly() bool { return state.failed || state.drift || state.config }

type yardGate struct {
	yards map[string]*yardGateState
	order []string
	// everyYard is set when the default yard's own configuration is invalid:
	// it underlies every yard, so each yard admits only its repair commands.
	everyYard bool
}

func (gate yardGate) state(yard string) yardGateState {
	state := yardGateState{}
	if found := gate.yards[yard]; found != nil {
		state = *found
	}
	if gate.everyYard {
		state.config = true
	}
	return state
}

// yardScopedOwners are the activation owners whose state is per yard.
func (cli *CLI) yardScopedOwners() []string {
	return append(cli.profileRuntimeIDs(), "core-guest-runtime", "materialized-config")
}

// yardScopedGate classifies a refused gate. It succeeds only when every cause
// is attributed to yards and the completed history is clean.
func (cli *CLI) yardScopedGate(outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) (yardGate, bool) {
	gate := yardGate{yards: map[string]*yardGateState{}}
	if outcome == nil || inspection == nil || inspection.Outcome != outcome || inspection.Resume != nil ||
		outcome.ReachedGoal || outcome.Active == "" || outcome.Active != outcome.Target {
		return gate, false
	}
	migrationRequired := outcome.Status == releasetransition.StatusMigrationRequired &&
		outcome.Code == releasetransition.CodeTransitionRequired && outcome.Transaction == nil
	operatorAction := outcome.Status == releasetransition.StatusOperatorActionRequired &&
		outcome.Code == releasetransition.CodeDependencyUnavailable
	if !migrationRequired && !operatorAction {
		return gate, false
	}
	type attributed struct{ kind, yard string }
	attribution := map[string][]attributed{}
	drift := map[string]bool{}
	for _, decision := range inspection.Decisions {
		switch {
		case decision.Scope == releasetransition.YardAttributionScope:
			owner, ok := activationOwner(decision.Resource)
			kind, yard, found := strings.Cut(decision.Result, " ")
			if !ok || !found || decision.Decision != releasetransition.DecisionPreserve || !domain.SafeName(yard) ||
				!slices.Contains([]string{releasetransition.YardFindingDrift, releasetransition.YardFindingFailed,
					releasetransition.YardFindingPending, releasetransition.YardFindingConfig}, kind) {
				return gate, false
			}
			attribution[owner] = append(attribution[owner], attributed{kind, yard})
		case decision.Decision == releasetransition.DecisionPreserve:
		case decision.Decision == releasetransition.DecisionCanonicalize && decision.Scope == "activation":
			owner, ok := activationOwner(decision.Resource)
			if !ok {
				return gate, false
			}
			drift[owner] = true
		default:
			return gate, false
		}
	}
	if migrationRequired && len(drift) == 0 || operatorAction && len(inspection.Blockers) == 0 {
		return gate, false
	}
	yardScoped := cli.yardScopedOwners()
	owned := func(owner string, kinds ...string) bool {
		matched := false
		for _, item := range attribution[owner] {
			if !slices.Contains(yardScoped, owner) && item.kind != releasetransition.YardFindingConfig {
				return false // host owners are never one yard's, except a yard's invalid config
			}
			matched = matched || slices.Contains(kinds, item.kind)
		}
		return matched
	}
	for owner := range drift {
		if !owned(owner, releasetransition.YardFindingDrift) {
			return gate, false
		}
	}
	for _, blocker := range inspection.Blockers {
		if blocker.Code != releasetransition.CodeDependencyUnavailable {
			return gate, false
		}
		if owner, ok := activationOwner(strings.TrimSuffix(blocker.Resource, ".pending-configuration")); ok && strings.HasSuffix(blocker.Resource, ".pending-configuration") {
			if !owned(owner, releasetransition.YardFindingPending) {
				return gate, false
			}
			continue
		}
		owner, ok := activationOwner(blocker.Resource)
		if !ok || !owned(owner, releasetransition.YardFindingFailed, releasetransition.YardFindingConfig) {
			return gate, false
		}
	}
	if !cli.completedHistoryClean(*outcome) {
		return gate, false
	}
	for owner, items := range attribution {
		for _, item := range items {
			state := gate.yards[item.yard]
			if state == nil {
				state = &yardGateState{}
				gate.yards[item.yard] = state
				gate.order = append(gate.order, item.yard)
			}
			switch item.kind {
			case releasetransition.YardFindingDrift:
				state.drift = drift[owner] || state.drift
			case releasetransition.YardFindingFailed:
				state.failed = true
			case releasetransition.YardFindingPending:
				state.pending = true
			case releasetransition.YardFindingConfig:
				state.config = true
				gate.everyYard = gate.everyYard || item.yard == "default"
			}
		}
	}
	slices.Sort(gate.order)
	if index := slices.Index(gate.order, "default"); index > 0 {
		gate.order = append([]string{"default"}, slices.Delete(gate.order, index, index+1)...)
	}
	for _, blocker := range inspection.Blockers {
		for _, yard := range gate.order {
			if strings.HasPrefix(blocker.Message, "yard "+yard+":") || strings.HasPrefix(blocker.Message, "yard "+yard+",") ||
				strings.HasPrefix(blocker.Message, "yard "+yard+" ") {
				state := gate.yards[yard]
				if !slices.Contains(state.messages, blocker.Message) {
					state.messages = append(state.messages, blocker.Message)
				}
				if state.retry == "" && strings.HasSuffix(blocker.Resource, ".pending-configuration") {
					state.retry = blocker.Retry
				}
				break
			}
		}
	}
	return gate, true
}

// activationOwner parses exactly activation.<owner> with a dotless safe owner.
func activationOwner(resource string) (string, bool) {
	owner, found := strings.CutPrefix(resource, "activation.")
	return owner, found && domain.SafeID(owner) && !strings.Contains(owner, ".")
}

// completedHistoryClean reads the protected journal and ledger itself: the
// completed journal of this exact release, and no pending migration.
func (cli *CLI) completedHistoryClean(outcome releasetransition.Outcome) bool {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return false
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return false
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return false
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || journal.Checkpoint != releasetransition.JournalComplete ||
		journal.Goal.Target != outcome.Active || outcome.Transaction != nil && *outcome.Transaction != journal.Transaction {
		return false
	}
	registryPayload, err := readConfigApplyRegistry(cli.options.RepositoryRoot)
	if err != nil {
		return false
	}
	catalog := releasetransition.BuiltinCapabilityCatalog()
	registry, registryDigest, err := releasetransition.ParseRegistryV2(registryPayload, catalog)
	if err != nil || registryDigest != journal.RegistryDigest {
		return false
	}
	ledger, err := store.ReadMigrationLedger(registry, registryDigest)
	if err != nil || !ledger.AuthoritySnapshot.Exists {
		return false
	}
	pending, err := registry.PendingPath(ledger.Ledger)
	return err == nil && len(pending) == 0
}

// hostCommandHandlers act on the host, not on one yard, and stay open.
var hostCommandHandlers = []string{"@host", "@keys", "@test-vms", "@network", "@client-projects", "@project-state"}

// yardRepairInvocation is the repair set of a yard that is not converged.
func (cli *CLI) yardRepairInvocation(handler string, arguments []string, definition resource.Definition, profileResource bool) bool {
	if profileResource {
		invocation, err := parseResourceInvocation(arguments)
		return err == nil && definition.Shutdown != "" && invocation.verb == definition.Shutdown
	}
	switch handler {
	case "@init", "@teardown", "@lifecycle", "@config", "@shell":
		return true
	case "@profile", "@integration":
		for _, argument := range arguments {
			if !strings.HasPrefix(argument, "-") {
				return argument == "disable"
			}
		}
	}
	return false
}

// admits decides one invocation on the selected yard.
func (cli *CLI) yardGateAdmits(gate yardGate, yard, handler string, arguments []string, definition resource.Definition, profileResource bool) bool {
	if !profileResource && slices.Contains(hostCommandHandlers, handler) {
		return true
	}
	state := gate.state(yard)
	switch {
	case state.repairOnly():
		return cli.yardRepairInvocation(handler, arguments, definition, profileResource)
	case state.pending:
		// The pending-configuration contract: unrelated commands, the yard's
		// plain init and profile resource verbs such as orca remove.
		if handler == "@init" {
			request, err := parseInitArguments(arguments)
			return err == nil && request.mode == initReconcile && request.profile == ""
		}
		return profileResource || ordinaryCommandDuringPendingConfiguration(handler)
	default:
		return true
	}
}

// yardNextStep is the one next step of an implicated yard.
func yardNextStep(yard string, state yardGateState) string {
	switch {
	case state.config:
		return "correct the yard's configuration, then yard -Y " + yard + " config validate"
	case state.failed:
		return "yard -Y " + yard + " init"
	case state.drift:
		return "yard migrate"
	case state.retry != "":
		return state.retry
	}
	return "yard -Y " + yard + " init"
}

// yardGateRefusal is the one text refusal of a yard that admits only repair.
func yardGateRefusal(yard string, gate yardGate) string {
	state := gate.state(yard)
	cause := "the release is active and complete, but this yard needs attention"
	if len(state.messages) != 0 {
		cause = state.messages[0]
		if extra := len(state.messages) - 1; extra > 0 {
			cause += fmt.Sprintf(" (and %d more; yard migrate --check)", extra)
		}
	} else if state.drift && !gate.everyYard {
		cause = "its release runtime is not repaired yet, for example after it was started past an update"
	} else if gate.everyYard && yard != "default" {
		cause = "the default yard's configuration is invalid; every yard admits only its repair commands"
		yard = "default"
		state = gate.state("default")
	}
	return fmt.Sprintf("release gate: yard %s needs repair: %s\nnext: %s\nuntil then only its repair commands run: init, init --reset, teardown, start, stop, shell, config, profile or integration disable and resource shutdown",
		yard, cause, yardNextStep(yard, state))
}

// gateAdmitsOrdinary is the one admission the direct CLI and RPC share before
// any bounded recovery: the yard-scoped gate, then pending configuration.
func (cli *CLI) gateAdmitsOrdinary(yard, handler string, arguments []string, outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) bool {
	if gate, ok := cli.yardScopedGate(outcome, inspection); ok {
		return cli.yardGateAdmits(gate, yard, handler, arguments, resource.Definition{}, false)
	}
	return cli.ordinaryCommandAdmitted(handler, outcome, inspection)
}

// otherYardFindings reports each other implicated yard once, with its step.
func (gate yardGate) otherYardFindings(yard string) []releasetransition.Blocker {
	var findings []releasetransition.Blocker
	for _, name := range gate.order {
		if name == yard {
			continue
		}
		state := gate.state(name)
		message := "yard " + name + " needs attention"
		if len(state.messages) != 0 {
			message = state.messages[0]
		}
		findings = append(findings, releasetransition.Blocker{Code: releasetransition.CodeDependencyUnavailable, Message: message, Retry: yardNextStep(name, state)})
	}
	return findings
}

// skipGatedYards leaves out, and reports once, the other yards a multi-yard
// verb must not touch while they admit only their own repair commands.
func (cli *CLI) skipGatedYards(targets []configTarget, selected string, report bool) []configTarget {
	if cli.gateYards == nil {
		return targets
	}
	return slices.DeleteFunc(slices.Clone(targets), func(target configTarget) bool {
		state := cli.gateYards.state(target.Name)
		if target.Name == selected || !state.repairOnly() {
			return false
		}
		if report {
			fmt.Fprintf(cli.options.Stderr, "yard: skipped yard %s: it needs repair; next: %s\n", target.Name, yardNextStep(target.Name, state))
		}
		return true
	})
}

// unfinishedActivationStop admits stopping one local yard while an activation
// of the active release is unfinished. A stopped yard is deferred like any
// stopped yard, so the release can complete without it; stop keeps its own
// session guards, and --force its own consent. Nothing else is admitted there.
func (cli *CLI) unfinishedActivationStop(ctx context.Context, yard string, outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) bool {
	if outcome.Status != releasetransition.StatusRecovering || outcome.Active == "" || outcome.Active != outcome.Target ||
		inspection == nil || inspection.Outcome != outcome || inspection.Resume == nil {
		return false
	}
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return false
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return false
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return false
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || journal.Checkpoint != releasetransition.JournalReconciling || journal.Transaction != *inspection.Resume ||
		journal.Goal.Target != outcome.Active {
		return false
	}
	loaded, err := cli.loadContext(yard)
	return err == nil && loaded.Context.AccessKind == domain.AccessLocal
}
