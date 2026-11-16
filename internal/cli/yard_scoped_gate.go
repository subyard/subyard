package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
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
	failed, drift, pending, config, unreachable bool
	// messages are the yard's distinct causes in report order.
	messages []string
	retry    string
}

// repairOnly reports a yard that admits only its repair commands.
func (state yardGateState) repairOnly() bool {
	return state.failed || state.drift || state.config || state.unreachable
}

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
					releasetransition.YardFindingPending, releasetransition.YardFindingConfig, releasetransition.YardFindingUnreachable}, kind) {
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
		if !ok || !owned(owner, releasetransition.YardFindingFailed, releasetransition.YardFindingConfig, releasetransition.YardFindingUnreachable) {
			return gate, false
		}
	}
	if !cli.completedHistoryClean(*outcome) {
		return gate, false
	}
	for _, items := range attribution {
		for _, item := range items {
			state := gate.yards[item.yard]
			if state == nil {
				state = &yardGateState{}
				gate.yards[item.yard] = state
				gate.order = append(gate.order, item.yard)
			}
			switch item.kind {
			case releasetransition.YardFindingDrift:
				state.drift = true
			case releasetransition.YardFindingFailed:
				state.failed = true
			case releasetransition.YardFindingUnreachable:
				state.unreachable = true
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
	case "@init":
		// Plain init repeats the yard's whole convergence and --reset rebuilds it;
		// --configs, --profile, --adopt and --continue-legacy change what the yard
		// is or does only part of the work, so they wait until it is converged.
		request, err := parseInitArguments(arguments)
		return err == nil && !request.adopt && !request.continueLegacy && request.profile == "" &&
			(request.mode == initReconcile || request.mode == initReset)
	case "@teardown", "@lifecycle", "@config", "@shell":
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
	// A host or shared setting reaches every yard's desired configuration, so it
	// waits while any yard's pending contract refuses configuration.
	if !profileResource && handler == "@config" && configWritesEveryYard(arguments) {
		for _, name := range gate.order {
			if other := gate.state(name); other.pending && !other.repairOnly() {
				return false
			}
		}
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
	case state.unreachable:
		return "yard migrate; if the guest agent still does not answer: yard -Y " + yard + " stop --force, yard -Y " + yard + " start, yard migrate"
	case state.failed:
		return "yard -Y " + yard + " init"
	case state.drift:
		return "yard -Y " + yard + " init"
	case state.retry != "":
		return state.retry
	}
	return "yard -Y " + yard + " init"
}

// yardGateRefusal is the one text refusal of a yard that admits only repair.
func yardGateRefusal(yard string, gate yardGate) string {
	state := gate.state(yard)
	if !state.repairOnly() && !state.pending {
		// Only a host or shared setting is refused for another yard's sake.
		for _, name := range gate.order {
			if other := gate.state(name); other.pending {
				return fmt.Sprintf("release gate: a host or shared setting reaches yard %s, whose pending configuration refuses it\nnext: %s, then repeat this setting",
					name, yardNextStep(name, other))
			}
		}
	}
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

// gateRequest is one invocation as the release gate decides it.
type gateRequest struct {
	yard, handler   string
	arguments       []string
	definition      resource.Definition
	profileResource bool
	stop            bool
}

// rpcGateRequest is the RPC form of an invocation, decided like the CLI's:
// profile resource verbs keep their definition and stop keeps its exit.
func (cli *CLI) rpcGateRequest(yard string, definition command.Definition, name string, arguments []string) gateRequest {
	request := gateRequest{yard: yard, handler: definition.Handler, arguments: arguments,
		stop: definition.Handler == "@lifecycle" && definition.Name == "stop" && !commandHelpRequested(arguments)}
	if resourceDefinition, found := cli.resources.Lookup(name); found && definition.Handler == "@resource" {
		request.handler, request.definition, request.profileResource = "", resourceDefinition, true
	}
	return request
}

// gateAdmission is how a non-ready gate admitted an invocation: by the
// yard-scoped gate, or as the bounded stop over an unfinished activation.
type gateAdmission struct {
	request gateRequest
	gate    *yardGate
}

// decideGate is the one admission of the direct CLI and RPC behind a non-ready
// gate. scoped reports a yard-scoped refusal the caller renders by yard.
func (cli *CLI) decideGate(ctx context.Context, request gateRequest, outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) (*gateAdmission, yardGate, bool) {
	if request.stop && cli.unfinishedActivationStop(ctx, request.yard, outcome, inspection) {
		return &gateAdmission{request: request}, yardGate{}, false
	}
	gate, ok := cli.yardScopedGate(outcome, inspection)
	if !ok {
		return nil, gate, false
	}
	if cli.yardGateAdmits(gate, request.yard, request.handler, request.arguments, request.definition, request.profileResource) {
		return &gateAdmission{request: request, gate: &gate}, gate, true
	}
	return nil, gate, true
}

// lockAdmittedRepair revalidates, after consent and before any effect, the
// admission of an invocation behind a non-ready gate. It holds the protected
// release lock that migrate and update hold, so the command never runs over a
// transition that started meanwhile; the caller releases it after its effects.
func (cli *CLI) lockAdmittedRepair(ctx context.Context) (context.Context, func(), error) {
	if cli.gateAdmitted == nil || ctx.Value(releaseLockHeldKey{}) != nil {
		return ctx, func() {}, nil
	}
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return ctx, nil, fmt.Errorf("%w: release state is unavailable for the admitted repair", domain.ErrPlanStale)
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return ctx, nil, err
	}
	unlock, err := store.Lock()
	if err != nil {
		return ctx, nil, err
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, cli.gateAdmitted.request.yard)
	if err == nil && outcome != nil {
		if admitted, _, _ := cli.decideGate(ctx, cli.gateAdmitted.request, outcome, inspection); admitted == nil {
			err = fmt.Errorf("%w: the release changed after confirmation: %s", domain.ErrPlanStale, strings.TrimSpace(mutationGateRefusalText(outcome, inspection)))
		} else {
			cli.gateAdmitted = admitted
		}
	}
	if err != nil {
		unlock()
		return ctx, nil, err
	}
	return context.WithValue(ctx, releaseLockHeldKey{}, true), unlock, nil
}

// releaseLockHeldKey marks a context whose invocation holds the protected
// release lock; nested bounded repairs reuse it instead of waiting on it.
type releaseLockHeldKey struct{}

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
// verb must not touch: those limited to repair and those whose pending
// contract refuses configuration.
func (cli *CLI) skipGatedYards(targets []configTarget, selected string, report bool) []configTarget {
	if cli.gateAdmitted == nil || cli.gateAdmitted.gate == nil {
		return targets
	}
	gate := cli.gateAdmitted.gate
	return slices.DeleteFunc(slices.Clone(targets), func(target configTarget) bool {
		state := gate.state(target.Name)
		if target.Name == selected || !state.repairOnly() && !state.pending {
			return false
		}
		if report {
			fmt.Fprintf(cli.options.Stderr, "yard: skipped yard %s: it needs attention; next: %s\n", target.Name, yardNextStep(target.Name, state))
		}
		return true
	})
}

// skipInvalidYards keeps a multi-yard verb going when other yards' own
// configuration cannot be loaded: those yards are skipped and reported. The
// selected yard's, or any other, refusal stays the verb's error.
func (cli *CLI) skipInvalidYards(targets []configTarget, err error, loaded config.Loaded, report bool) ([]configTarget, error) {
	if err == nil {
		return targets, nil
	}
	home := loaded.Context.Paths.ConfigHome
	findings, ok := activationYardFindings(err, "", true, home)
	for _, finding := range findings {
		ok = ok && finding.Kind == releasetransition.YardFindingConfig && finding.Yard != loaded.Context.YardName && finding.Yard != "default"
	}
	if !ok {
		return targets, err
	}
	if report {
		reported := map[string]bool{}
		for _, finding := range findings {
			if !reported[finding.Yard] {
				reported[finding.Yard] = true
				fmt.Fprintf(cli.options.Stderr, "yard: skipped yard %s: its configuration is invalid; next: yard -Y %s config validate\n", finding.Yard, finding.Yard)
			}
		}
	}
	return targets, nil
}

// statusYardGate classifies the local release for status when it is neither
// ready nor unknown; only then is the release reported once for all yards.
func (cli *CLI) statusYardGate(ctx context.Context, readiness *domain.ReleaseReadiness) (yardGate, bool) {
	if readiness == nil || readiness.Status == string(releasetransition.StatusReady) || readiness.Status == "unknown" {
		return yardGate{}, false
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, "default")
	if err != nil || outcome == nil {
		return yardGate{}, false
	}
	return cli.yardScopedGate(outcome, inspection)
}

// releaseLine states the release once; the protocol word ready is never used
// for a non-ready status.
func (gate yardGate) releaseLine() string {
	names := gate.order
	if gate.everyYard {
		names = []string{"default"}
	}
	subject := "yard " + strings.Join(names, ", ")
	if len(names) > 1 {
		subject = "yards " + strings.Join(names, ", ")
	}
	return "release  active and complete; " + subject + " needs attention"
}

// printYardAttention prints one yard's findings under it, with its next step.
func (cli *CLI) printYardAttention(gate yardGate, yard string) {
	state := gate.state(yard)
	if _, implicated := gate.yards[yard]; !implicated && !gate.everyYard {
		return
	}
	if gate.everyYard && yard != "default" {
		fmt.Fprintln(cli.options.Stdout, "  attention the default yard's configuration is invalid; it underlies this yard")
		fmt.Fprintf(cli.options.Stdout, "    next: %s\n", yardNextStep("default", gate.state("default")))
		return
	}
	messages := state.messages
	if len(messages) == 0 {
		messages = []string{"this yard needs attention"}
	}
	for _, message := range messages {
		fmt.Fprintf(cli.options.Stdout, "  attention %s\n", message)
	}
	fmt.Fprintf(cli.options.Stdout, "    next: %s\n", yardNextStep(yard, state))
}

// unfinishedActivationStop admits stopping one local yard while an activation
// of the active release is unfinished. A stopped yard is deferred like any
// stopped yard, so the release can complete without it; stop keeps its own
// session guards, and --force its own consent. Nothing else is admitted there.
func (cli *CLI) unfinishedActivationStop(ctx context.Context, yard string, outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) bool {
	// An unfinished journal reports recovering, or operator-action-required
	// when an observation, such as of a lost guest agent, is refused.
	if outcome.Status != releasetransition.StatusRecovering && outcome.Status != releasetransition.StatusOperatorActionRequired ||
		outcome.Active == "" || outcome.Active != outcome.Target ||
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

// configWritesEveryYard reports a configuration write at host or shared scope.
func configWritesEveryYard(arguments []string) bool {
	arguments = configArguments(arguments)
	if len(arguments) == 0 || !slices.Contains([]string{"set", "unset"}, arguments[0]) {
		return false
	}
	request, err := parseConfigAuthoringRequest(arguments[0], arguments[1:], false)
	return err != nil || request.git || request.scope == config.ScopeHost || request.scope == config.ScopeShared
}

// repairOnlyYards names, in order, the given yards the admitting gate limits
// to their own repair commands.
func (cli *CLI) repairOnlyYards(names []string) []string {
	if cli.gateAdmitted == nil || cli.gateAdmitted.gate == nil {
		return nil
	}
	var blocked []string
	for _, name := range names {
		if cli.gateAdmitted.gate.state(name).repairOnly() {
			blocked = append(blocked, name)
		}
	}
	return blocked
}

func (cli *CLI) repairNextStep(yard string) string {
	return yardNextStep(yard, cli.gateAdmitted.gate.state(yard))
}
