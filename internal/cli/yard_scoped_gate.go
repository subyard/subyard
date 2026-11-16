package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
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
	return cli.yardScopedOwnersFor(cli.profileRuntimeIDs())
}

func (*CLI) yardScopedOwnersFor(profileRuntimes []string) []string {
	return append(slices.Clone(profileRuntimes), "core-guest-runtime", "materialized-config")
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
	// An aggregate blocker joins its yards' findings; each yard shows its own
	// part. The text only groups messages for display, never for admission.
	for _, blocker := range inspection.Blockers {
		parts := strings.Split(blocker.Message, "; yard ")
		for index := 1; index < len(parts); index++ {
			parts[index] = "yard " + parts[index]
		}
		for _, part := range parts {
			for _, yard := range gate.order {
				if strings.HasPrefix(part, "yard "+yard+":") || strings.HasPrefix(part, "yard "+yard+",") ||
					strings.HasPrefix(part, "yard "+yard+" ") {
					state := gate.yards[yard]
					if !slices.Contains(state.messages, part) {
						state.messages = append(state.messages, part)
					}
					if state.retry == "" && strings.HasSuffix(blocker.Resource, ".pending-configuration") {
						state.retry = blocker.Retry
					}
					break
				}
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
	if err != nil || registryDigest != journal.RegistryDigest || catalog.Digest() != journal.CatalogDigest {
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

// yardGateAdmits decides one invocation on the selected yard. repair reports
// an admission through the repair-only branch, which carries a post-consent
// recheck when its member has consent.
func (cli *CLI) yardGateAdmits(gate yardGate, request gateRequest) (admitted, repair bool) {
	if !request.profileResource && slices.Contains(hostCommandHandlers, request.handler) {
		return true, false
	}
	// A host or shared setting reaches every yard's desired configuration, so it
	// waits while any yard's pending contract refuses configuration.
	if !request.profileResource && request.handler == "@config" && configWritesEveryYard(request.arguments) {
		for _, name := range gate.order {
			if gate.state(name).pending {
				return false, false
			}
		}
	}
	state := gate.state(request.yard)
	switch {
	case state.repairOnly():
		entry, ok := cli.yardRepair(request)
		return ok, ok && entry.consent
	case state.pending:
		// The pending-configuration contract: unrelated commands, the yard's
		// plain init and profile resource verbs such as orca remove.
		if request.handler == "@init" && !request.profileResource {
			parsed, err := parseInitArguments(request.arguments)
			return err == nil && parsed.mode == initReconcile && parsed.profile == "", false
		}
		return request.profileResource || ordinaryCommandDuringPendingConfiguration(request.handler), false
	default:
		return true, false
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

// yardRepairSetText names the repair set in its order.
func yardRepairSetText() string {
	names := make([]string, 0, len(yardRepairSet))
	for _, entry := range yardRepairSet {
		names = append(names, entry.name)
	}
	return strings.Join(names, ", ")
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
	return fmt.Sprintf("release gate: yard %s needs repair: %s\nnext: %s\nuntil then only its repair commands run: %s",
		yard, cause, yardNextStep(yard, state), yardRepairSetText())
}

// gateRequest is one invocation as the release gate decides it.
type gateRequest struct {
	yard, handler, name string
	arguments           []string
	definition          resource.Definition
	profileResource     bool
}

// rpcGateRequest is the RPC form of an invocation, decided like the CLI's:
// profile resource verbs keep their definition.
func (cli *CLI) rpcGateRequest(yard string, definition command.Definition, arguments []string) gateRequest {
	request := gateRequest{yard: yard, handler: definition.Handler, name: definition.Name, arguments: arguments}
	if resourceDefinition, found := cli.resources.Lookup(definition.Name); found && definition.Handler == "@resource" {
		request.handler, request.definition, request.profileResource = "", resourceDefinition, true
	}
	return request
}

// gateAdmission is how a non-ready gate admitted an invocation. A repair
// admission, of a repair command on a yard that needs repair or of the
// bounded stop over an unfinished activation, carries the release history it
// was decided on; preparedCommand.Execute rechecks it once under the owner
// barrier. Every other admission is decided once, as on a ready release.
type gateAdmission struct {
	request gateRequest
	gate    *yardGate
	repair  bool
	// bounded marks the stop admitted over an unfinished activation.
	bounded bool
	history releaseHistoryToken
	// sources binds the running tooling to the installed source of the
	// completed release, for every repair but the bounded stop.
	sources   *repairSourceBinding
	rechecked bool
}

// decideGate is the one admission decision of the direct CLI and RPC behind a
// non-ready gate. scoped reports a yard-scoped refusal the caller renders by
// yard.
func (cli *CLI) decideGate(ctx context.Context, request gateRequest, outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) (*gateAdmission, yardGate, bool) {
	if cli.boundedStopRequest(request) && cli.unfinishedActivationStop(ctx, request.yard, outcome, inspection) {
		return &gateAdmission{request: request, repair: true, bounded: true}, yardGate{}, false
	}
	gate, ok := cli.yardScopedGate(outcome, inspection)
	if !ok {
		return nil, gate, false
	}
	admitted, repair := cli.yardGateAdmits(gate, request)
	if !admitted {
		return nil, gate, true
	}
	return &gateAdmission{request: request, gate: &gate, repair: repair}, gate, true
}

// admitGate is the one constructor of a gate admission. It reads the release
// history before and after the inspection; a repair admission requires both
// to agree, so its decision and token describe the same history. An outcome
// of nil is a ready release, admitted as before.
func (cli *CLI) admitGate(
	ctx context.Context, request gateRequest,
	inspect func() (*releasetransition.Outcome, *releasetransition.Inspection, error),
) (admitted *gateAdmission, outcome *releasetransition.Outcome, inspection *releasetransition.Inspection, gate yardGate, scoped bool, err error) {
	before, beforeErr := cli.readReleaseHistory()
	outcome, inspection, err = inspect()
	if err != nil || outcome == nil {
		return nil, outcome, inspection, gate, false, err
	}
	admitted, gate, scoped = cli.decideGate(ctx, request, outcome, inspection)
	if admitted == nil || !admitted.repair {
		return admitted, outcome, inspection, gate, scoped, nil
	}
	after, afterErr := cli.readReleaseHistory()
	if err = errors.Join(beforeErr, afterErr); err != nil {
		return nil, outcome, inspection, gate, scoped, err
	}
	if before == nil || after == nil || *before != *after {
		return nil, outcome, inspection, gate, scoped, fmt.Errorf("%w: the release changed during admission; rerun the command", domain.ErrPlanStale)
	}
	admitted.history = *after
	if !admitted.bounded {
		if admitted.sources, err = cli.repairSources(ctx); err != nil {
			return nil, outcome, inspection, gate, scoped, err
		}
	}
	return admitted, outcome, inspection, gate, scoped, nil
}

// yardRepairEntry is one member of the repair set of a yard that is not
// converged. A member with consent runs its effects through
// preparedCommand.Execute, which rechecks its admission under the owner
// barrier; shell has no prepared execution, so its gate admission is its only
// check.
type yardRepairEntry struct {
	name    string
	consent bool
	admits  func(*CLI, gateRequest) bool
}

// yardRepairSet is the closed repair set: plain init repeats the yard's whole
// convergence and --reset rebuilds it, while --configs, --profile, --adopt and
// --continue-legacy change what the yard is or do only part of the work; the
// other members stop, remove, configure or diagnose the yard. Configuration
// admits its prepared verbs only, which pass the recheck; a resource admits
// only its declared shutdown with a prepared plan.
var yardRepairSet = []yardRepairEntry{
	{"init", true, func(_ *CLI, request gateRequest) bool { return initRepairMode(request, initReconcile) }},
	{"init --reset", true, func(_ *CLI, request gateRequest) bool { return initRepairMode(request, initReset) }},
	{"teardown", true, func(_ *CLI, request gateRequest) bool { return request.handler == "@teardown" }},
	{"start", true, func(_ *CLI, request gateRequest) bool {
		return request.handler == "@lifecycle" && request.name == "start"
	}},
	{"stop", true, func(_ *CLI, request gateRequest) bool {
		return request.handler == "@lifecycle" && request.name == "stop"
	}},
	{"shell", false, func(_ *CLI, request gateRequest) bool { return request.handler == "@shell" }},
	{"config", true, func(cli *CLI, request gateRequest) bool {
		return request.handler == "@config" && configRepairInvocation(cli, request.arguments)
	}},
	{"profile disable", true, func(_ *CLI, request gateRequest) bool {
		return request.handler == "@profile" && firstVerb(request.arguments) == "disable"
	}},
	{"integration disable", true, func(_ *CLI, request gateRequest) bool {
		return request.handler == "@integration" && firstVerb(request.arguments) == "disable"
	}},
	{"resource shutdown", true, func(cli *CLI, request gateRequest) bool {
		invocation, err := parseResourceInvocation(request.arguments)
		return request.profileResource && err == nil && request.definition.Shutdown != "" && invocation.verb == request.definition.Shutdown &&
			cli.resourceExactInvocation(request.definition, request.arguments)
	}},
}

// configRepairInvocation is a configuration verb that runs as a prepared
// command: exact apply and sync, and local set, unset, import and edit. Git
// authoring and the other verbs stay refused on a yard that needs repair.
func configRepairInvocation(cli *CLI, arguments []string) bool {
	verbs := configArguments(arguments)
	if len(verbs) == 0 || commandHelpRequested(verbs) {
		return false
	}
	switch verbs[0] {
	case "set", "unset", "import", "edit":
		requests, ok := configAuthoringRequests(verbs)
		if !ok || len(requests) == 0 || (verbs[0] == "import" || verbs[0] == "edit") && len(requests) != 1 {
			return false
		}
		for _, request := range requests {
			if request.git {
				return false
			}
		}
		return true
	case "apply", "sync":
		return configExactInvocation(config.Loaded{Catalog: cli.catalog}, arguments)
	}
	return false
}

// configAuthoringRequests parses the authoring requests of a set, unset,
// import or edit invocation.
func configAuthoringRequests(verbs []string) ([]configAuthoringRequest, bool) {
	requests, err := parseConfigAuthoringRequests(verbs[0], verbs[1:], false)
	return requests, err == nil
}

func initRepairMode(request gateRequest, mode initMode) bool {
	if request.handler != "@init" {
		return false
	}
	parsed, err := parseInitArguments(request.arguments)
	return err == nil && !parsed.adopt && !parsed.continueLegacy && parsed.profile == "" && parsed.mode == mode
}

func firstVerb(arguments []string) string {
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, "-") {
			return argument
		}
	}
	return ""
}

// yardRepair returns the repair set member an invocation is, if any.
func (cli *CLI) yardRepair(request gateRequest) (yardRepairEntry, bool) {
	if commandHelpRequested(request.arguments) {
		return yardRepairEntry{}, false
	}
	for _, entry := range yardRepairSet {
		if entry.admits(cli, request) {
			return entry, true
		}
	}
	return yardRepairEntry{}, false
}

// boundedStopRequest is stop of one yard, the one exit over an unfinished activation.
func (cli *CLI) boundedStopRequest(request gateRequest) bool {
	return !request.profileResource && request.handler == "@lifecycle" && request.name == "stop" && !commandHelpRequested(request.arguments)
}

// releaseHistoryToken identifies the protected release history: journal,
// ledger compatibility projection and checkpoint, as journal writers change
// them under the owner barrier.
type releaseHistoryToken struct {
	journal, ledger, checkpoint string
	checkpointed                bool
}

func (cli *CLI) readReleaseHistory() (*releaseHistoryToken, error) {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return nil, err
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return nil, err
	}
	journal, err := store.ReadCurrentJournal()
	if err != nil {
		return nil, err
	}
	ledger, err := store.ReadLedger()
	if err != nil {
		return nil, err
	}
	checkpoint, err := store.ReadMigrationCheckpoint()
	if err != nil {
		return nil, err
	}
	return &releaseHistoryToken{journal: string(journal.Fingerprint), ledger: string(ledger.Fingerprint),
		checkpoint: string(checkpoint.Fingerprint), checkpointed: checkpoint.Exists}, nil
}

// repairSourceBinding is the installed source authority and tooling a repair
// runs with: the running CLI is the published runtime of the completed
// journal's target, and its source proof names that journal's transaction and
// artifact. A development build or another staged or retained runtime with
// the same registry and catalog is not that source.
type repairSourceBinding struct{ source, tooling string }

func (cli *CLI) repairSources(ctx context.Context) (*repairSourceBinding, error) {
	refused := fmt.Errorf("%w: a repair behind the release gate runs only with the installed release's own tooling; run it with the installed yard command", domain.ErrPlanStale)
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return nil, refused
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return nil, refused
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return nil, refused
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil || journal.Checkpoint != releasetransition.JournalComplete {
		return nil, refused
	}
	bind := cli.sourceBinding
	if bind == nil {
		bind = cli.completedSourceBindings
	}
	source, tooling, err := bind(ctx, journal)
	if err != nil {
		return nil, refused
	}
	return &repairSourceBinding{source: source, tooling: tooling}, nil
}

// completedSourceBindings binds the current source authority and tooling to a completed journal.
func (cli *CLI) completedSourceBindings(ctx context.Context, journal releasetransition.JournalRecord) (string, string, error) {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return "", "", domain.ErrPlanStale
	}
	inspector := releaseruntime.NewSourceInspector(cli.options.RepositoryRoot, cli.options.Stderr)
	defer inspector.Close()
	dir, tooling, err := inspector.InspectSourceTooling(ctx, options.RuntimeRoot, cli.options.RepositoryRoot)
	if err != nil || dir != filepath.Join(options.RuntimeRoot, "releases", string(journal.Goal.Target), "config") {
		return "", "", domain.ErrPlanStale
	}
	proof, err := cli.inspectConfigSourceAdmission(ctx, inspector)
	if err != nil || proof.Metadata.Journal == nil || proof.Metadata.Journal.Checkpoint != releasetransition.JournalComplete ||
		proof.Metadata.Journal.Transaction != journal.Transaction || proof.Metadata.Journal.ArtifactDigest != journal.ArtifactDigest {
		return "", "", domain.ErrPlanStale
	}
	return proof.Binding, tooling, nil
}

// recheck is the one post-consent check of a repair admission. It runs in
// preparedCommand.Execute right after the owner barrier is acquired, which
// every release journal writer holds: the history must be the one admitted,
// and a fresh inspection, which binds the installed source and runtime links
// to that history, must admit the invocation again. It runs once.
func (admission *gateAdmission) recheck(ctx context.Context, cli *CLI) error {
	if admission == nil || !admission.repair || admission.rechecked {
		return nil
	}
	if cli.ownerMutationLock == nil {
		return errors.New("a repair admitted behind the release gate runs only on the owner host under its owner barrier")
	}
	current, err := cli.readReleaseHistory()
	if err != nil {
		return err
	}
	if current == nil || *current != admission.history {
		return fmt.Errorf("%w: the release changed after confirmation; rerun the command", domain.ErrPlanStale)
	}
	if admission.sources != nil {
		sources, err := cli.repairSources(ctx)
		if err != nil || *sources != *admission.sources {
			return fmt.Errorf("%w: the installed release source changed after confirmation; rerun the command", domain.ErrPlanStale)
		}
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, admission.request.yard)
	if err != nil {
		return err
	}
	if outcome != nil {
		again, _, _ := cli.decideGate(ctx, admission.request, outcome, inspection)
		if again == nil {
			return fmt.Errorf("%w: the release changed after confirmation: %s", domain.ErrPlanStale, strings.TrimSpace(mutationGateRefusalText(outcome, inspection)))
		}
	}
	if current, err = cli.readReleaseHistory(); err != nil {
		return err
	}
	if current == nil || *current != admission.history {
		return fmt.Errorf("%w: the release changed after confirmation; rerun the command", domain.ErrPlanStale)
	}
	admission.rechecked = true
	return nil
}

// verifyAfterEffect checks, still under the owner barrier, that a repair left
// release history as admitted, and names what the yard still needs.
func (admission *gateAdmission) verifyAfterEffect(ctx context.Context, cli *CLI) error {
	if admission == nil || !admission.repair || !admission.rechecked {
		return nil
	}
	current, err := cli.readReleaseHistory()
	if err != nil {
		return err
	}
	if current == nil || *current != admission.history {
		return errors.New("the repair changed release history; run yard migrate --check")
	}
	if admission.bounded || admission.request.handler == "@init" || admission.request.handler == "@teardown" {
		return nil // init verifies its own readiness; a removed yard has none
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, admission.request.yard)
	if err != nil || outcome == nil {
		return nil
	}
	if gate, ok := cli.yardScopedGate(outcome, inspection); ok {
		if state := gate.state(admission.request.yard); state.repairOnly() || state.pending {
			fmt.Fprintf(cli.options.Stderr, "yard: yard %s still needs attention; next: %s\n", admission.request.yard, yardNextStep(admission.request.yard, state))
		}
	}
	return nil
}

// configConsumerAdmission admits the refresh a published configuration change
// needs on its own target yards when the gate is yard-scoped: each target is
// open or limited to repair only by drift, which this refresh repairs, and
// other yards' findings wait for their own repair. The admission applies in
// this process under the owner barrier the publishing command holds. It is
// nil when the gate is not yard-scoped or a target has another finding.
func (cli *CLI) configConsumerAdmission(ctx context.Context, yard string, names []string) (*gateAdmission, error) {
	if cli.ownerMutationLock == nil {
		return nil, nil
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, yard)
	if err != nil || outcome == nil {
		return nil, err
	}
	gate, ok := cli.yardScopedGate(outcome, inspection)
	if !ok {
		return nil, nil
	}
	for _, name := range names {
		state := gate.state(name)
		if state.failed || state.config || state.unreachable || state.pending {
			return nil, nil
		}
	}
	history, err := cli.readReleaseHistory()
	if err != nil || history == nil {
		return nil, err
	}
	// Like every repair, the refresh runs only with the installed tooling.
	sources, err := cli.repairSources(ctx)
	if err != nil {
		return nil, nil
	}
	return &gateAdmission{request: gateRequest{yard: yard, handler: "@config", name: "config"}, gate: &gate,
		repair: true, history: *history, sources: sources, rechecked: true}, nil
}

// guardEffects refuses an adapter while a repair admission has not passed its
// recheck, so no repair reaches an effect outside preparedCommand.Execute.
func (cli *CLI) guardEffects(context.Context) error {
	if admission := cli.gateAdmitted; admission != nil && admission.repair && !admission.rechecked {
		return errors.New("a repair admitted behind the release gate runs only through its confirmed plan")
	}
	return nil
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
// verb must not touch: those limited to repair and those whose pending
// contract refuses configuration.
func (cli *CLI) skipGatedYards(targets []configTarget, selected string, report bool) []configTarget {
	if cli.gateAdmitted == nil || cli.gateAdmitted.gate == nil {
		return targets
	}
	gate := cli.gateAdmitted.gate
	kept := slices.DeleteFunc(slices.Clone(targets), func(target configTarget) bool {
		state := gate.state(target.Name)
		if target.Name == selected || !state.repairOnly() && !state.pending {
			return false
		}
		if report {
			fmt.Fprintf(cli.options.Stderr, "yard: skipped yard %s: it needs attention; next: %s\n", target.Name, yardNextStep(target.Name, state))
		}
		return true
	})
	return kept
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

// configWritesEveryYard reports a configuration write at host or shared scope,
// or to Git, which every yard's desired configuration can read: set, unset,
// import and edit alike. A write that cannot be parsed is treated as one.
func configWritesEveryYard(arguments []string) bool {
	arguments = configArguments(arguments)
	if len(arguments) == 0 || !slices.Contains([]string{"set", "unset", "import", "edit"}, arguments[0]) {
		return false
	}
	requests, ok := configAuthoringRequests(arguments)
	if !ok || len(requests) == 0 {
		return true
	}
	for _, request := range requests {
		if request.git || request.scope == config.ScopeHost || request.scope == config.ScopeShared {
			return true
		}
	}
	return false
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
