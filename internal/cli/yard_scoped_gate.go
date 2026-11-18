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
	materializedDrift                           bool
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
	everyYard     bool
	sourceRefusal error
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
				state.drift = true
				state.materializedDrift = state.materializedDrift || owner == "materialized-config"
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

// admissionKind is how an invocation passed the release gate before
// dispatch. The kind travels with the prepared command; Execute decides
// on it under the owner barrier.
type admissionKind int

const (
	// admissionReady: the release is ready, or no gate applies.
	admissionReady admissionKind = iota
	// admissionRead: a reader that skipped the gate; it may run no effect.
	admissionRead
	// admissionExempt: the handler owns its own release admission.
	admissionExempt
	// admissionOpen: a non-ready yard-scoped gate admitted it as an ordinary
	// command; Execute decides its subject on the stored snapshot.
	admissionOpen
	// admissionRepair: a repair on a yard that needs repair; Execute rechecks
	// it against fresh release state.
	admissionRepair
	// admissionBounded: stop of one yard over an unfinished activation.
	admissionBounded
)

// gateAdmission is the admission of one invocation. A repair or bounded
// admission carries the release history it was decided on, and a repair the
// installed source binding; preparedCommand.Execute rechecks them once.
type gateAdmission struct {
	kind    admissionKind
	request gateRequest
	gate    *yardGate
	history releaseHistoryToken
	// sources binds the running tooling to the installed source of the
	// completed release, for every repair but the bounded stop.
	sources   *repairSourceBinding
	rechecked bool
}

func (admission *gateAdmission) repair() bool {
	return admission != nil && (admission.kind == admissionRepair || admission.kind == admissionBounded)
}

// decideGate is the one pre-dispatch admission decision of the direct CLI and
// RPC behind a non-ready gate. It decides by handler identity and the
// handler's own parser, on the selected yard only: what else a prepared
// command writes or acts on is its subject, decided before consent and again
// in Execute. refusal is a yard-scoped refusal; scoped is false for a
// host-wide one.
func (cli *CLI) decideGate(ctx context.Context, request gateRequest, outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) (*gateAdmission, yardGate, error, bool) {
	if cli.boundedStopRequest(request) && cli.unfinishedActivationStop(ctx, request.yard, outcome, inspection) {
		return &gateAdmission{kind: admissionBounded, request: request}, yardGate{}, nil, false
	}
	gate, ok := cli.yardScopedGate(outcome, inspection)
	if !ok {
		return nil, gate, nil, false
	}
	subject := cli.provisionalSubject(request)
	subject.scope, subject.named, subject.acts = scopeSelected, nil, nil
	verdict := gate.verdict(request.yard, subject)
	if err := verdict.err(); err != nil {
		return nil, gate, err, true
	}
	kind := admissionOpen
	if verdict.repair {
		kind = admissionRepair
	}
	return &gateAdmission{kind: kind, request: request, gate: &gate}, gate, nil, true
}

// provisionalSubject is the subject handler identity and the handler's real
// parser give before prepare. It is final for a non-prepared mutator; a
// prepared command replaces it with the subject its prepare attaches.
func (cli *CLI) provisionalSubject(request gateRequest) gateSubject {
	if request.profileResource {
		subject := gateSubject{declared: true, pending: true}
		if invocation, err := parseResourceInvocation(request.arguments); err == nil && request.definition.Shutdown != "" && invocation.verb == request.definition.Shutdown {
			subject.member = memberResourceShutdown
		}
		return subject
	}
	if subject, ok := nonPreparedMutators[request.handler]; ok {
		return subject
	}
	switch request.handler {
	case "@init":
		parsed, err := parseInitArguments(request.arguments)
		if err != nil {
			return gateSubject{declared: true, member: memberInit, unfit: err}
		}
		return initSubject(parsed)
	case "@teardown":
		return gateSubject{declared: true, member: memberTeardown, pending: true}
	case "@lifecycle":
		return lifecycleSubject(request.name)
	case "@config":
		// Authoring verbs read their requests with the handler's own parser.
		if verbs := configArguments(request.arguments); len(verbs) != 0 && slices.Contains([]string{"set", "unset", "import", "edit"}, verbs[0]) {
			if requests, err := parseConfigAuthoringRequests(verbs[0], verbs[1:], false); err == nil {
				return configAuthoringSubject(request.yard, requests)
			}
		}
		return gateSubject{declared: true, member: memberConfig}
	case "@profile":
		parsed, err := parseProfileArguments(request.arguments)
		if err != nil {
			return gateSubject{declared: true, member: memberProfileDisable, unfit: err}
		}
		return verbSubject(parsed.verb, memberProfileDisable)
	case "@integration":
		parsed, err := parseIntegrationArguments(request.arguments)
		if err != nil {
			return gateSubject{declared: true, member: memberIntegrationDisable, unfit: err}
		}
		return verbSubject(parsed.verb, memberIntegrationDisable)
	}
	if subject, ok := handlerSubjects[request.handler]; ok {
		return subject
	}
	return gateSubject{}
}

// admitGate is the one constructor of a gate admission. It reads the release
// history before and after the inspection; a repair admission requires both
// to agree, so its decision and token describe the same history. An outcome
// of nil is a ready release.
func (cli *CLI) admitGate(
	ctx context.Context, request gateRequest,
	inspect func() (*releasetransition.Outcome, *releasetransition.Inspection, error),
) (admitted *gateAdmission, outcome *releasetransition.Outcome, inspection *releasetransition.Inspection, refusal error, scoped bool, err error) {
	before, beforeErr := cli.readReleaseHistory()
	outcome, inspection, err = inspect()
	if err != nil {
		return nil, outcome, inspection, nil, false, err
	}
	if outcome == nil {
		return &gateAdmission{kind: admissionReady, request: request}, nil, inspection, nil, false, nil
	}
	admitted, _, refusal, scoped = cli.decideGate(ctx, request, outcome, inspection)
	if admitted == nil || !admitted.repair() {
		return admitted, outcome, inspection, refusal, scoped, nil
	}
	after, afterErr := cli.readReleaseHistory()
	if err = errors.Join(beforeErr, afterErr); err != nil {
		return nil, outcome, inspection, nil, scoped, err
	}
	if before == nil || after == nil || *before != *after {
		return nil, outcome, inspection, nil, scoped, fmt.Errorf("%w: the release changed during admission; rerun the command", domain.ErrPlanStale)
	}
	admitted.history = *after
	// A shell into a yard that needs repair diagnoses it with any tooling; its
	// history was still recorded above.
	if admitted.kind == admissionRepair && request.handler != "@shell" {
		if admitted.sources, err = cli.repairSources(ctx); err != nil {
			return nil, outcome, inspection, nil, scoped, fmt.Errorf("%w: %w; next: %s", domain.ErrPlanStale, err, repairSourceNext)
		}
	}
	return admitted, outcome, inspection, nil, scoped, nil
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

const repairSourceNext = "yard update"

func (cli *CLI) repairSources(ctx context.Context) (*repairSourceBinding, error) {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return nil, errors.New("installed release metadata is unavailable")
	}
	store, err := releasetransition.NewPOSIXV2Store(options.ConfigHome)
	if err != nil {
		return nil, errors.New("protected release store is unavailable")
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil {
		return nil, errors.New("current release journal could not be read")
	}
	if !snapshot.Exists {
		return nil, errors.New("current release journal is missing")
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	if err != nil {
		return nil, errors.New("current release journal is invalid")
	}
	if journal.Checkpoint != releasetransition.JournalComplete {
		return nil, errors.New("current release journal is unfinished")
	}
	bind := cli.sourceBinding
	if bind == nil {
		bind = cli.completedSourceBindings
	}
	source, tooling, err := bind(ctx, journal)
	if err != nil {
		return nil, err
	}
	return &repairSourceBinding{source: source, tooling: tooling}, nil
}

// completedSourceBindings binds the current source authority and tooling to a completed journal.
func (cli *CLI) completedSourceBindings(ctx context.Context, journal releasetransition.JournalRecord) (string, string, error) {
	options, available, err := cli.mutationGateReleaseOptions()
	if err != nil || !available {
		return "", "", errors.New("installed release metadata is unavailable")
	}
	inspector := releaseruntime.NewSourceInspector(cli.options.RepositoryRoot, cli.options.Stderr)
	defer inspector.Close()
	dir, tooling, err := inspector.InspectSourceTooling(ctx, options.RuntimeRoot, cli.options.RepositoryRoot)
	var refusals []error
	if err != nil || dir != filepath.Join(options.RuntimeRoot, "releases", string(journal.Goal.Target), "config") {
		refusals = append(refusals, errors.New("running tooling does not match the sealed completed release"))
	}
	proof, err := cli.inspectConfigSourceAdmission(ctx, inspector)
	switch {
	case err != nil:
		refusals = append(refusals, errors.New(releaseruntime.SourceAdmissionPredicate(err)))
	case proof.Metadata.Journal == nil:
		refusals = append(refusals, errors.New("installed release source proof has no journal"))
	default:
		if proof.Metadata.Journal.Checkpoint != releasetransition.JournalComplete {
			refusals = append(refusals, errors.New("installed release source proof names an unfinished journal"))
		}
		if proof.Metadata.Journal.Transaction != journal.Transaction {
			refusals = append(refusals, errors.New("installed release source proof transaction differs from the current journal"))
		}
		if proof.Metadata.Journal.ArtifactDigest != journal.ArtifactDigest {
			refusals = append(refusals, errors.New("installed release source proof artifact differs from the current journal"))
		}
	}
	return proof.Binding, tooling, domain.CollectRefusals(refusals...)
}

// recheck is the one post-consent check of a repair admission. It runs in
// preparedCommand.Execute right after the owner barrier is acquired, which
// every release journal writer holds: the history must be the one admitted,
// and a fresh inspection, which binds the installed source and runtime links
// to that history, must admit the invocation again. It runs once.
func (admission *gateAdmission) recheck(ctx context.Context, cli *CLI) error {
	if !admission.repair() || admission.rechecked {
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
		if err != nil {
			return fmt.Errorf("%w: %w; next: %s", domain.ErrPlanStale, err, repairSourceNext)
		}
		if *sources != *admission.sources {
			return fmt.Errorf("%w: the installed release source changed after confirmation; rerun the command", domain.ErrPlanStale)
		}
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, admission.request.yard)
	if err != nil {
		return err
	}
	if outcome != nil {
		again, gate, _, _ := cli.decideGate(ctx, admission.request, outcome, inspection)
		if again == nil {
			return fmt.Errorf("%w: the release changed after confirmation: %s", domain.ErrPlanStale, strings.TrimSpace(mutationGateRefusalText(outcome, inspection)))
		}
		if again.kind == admissionBounded {
			gate = yardGate{}
		}
		admission.gate = &gate
	} else {
		admission.gate = &yardGate{}
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
	if !admission.repair() || !admission.rechecked {
		return nil
	}
	current, err := cli.readReleaseHistory()
	if err != nil {
		return err
	}
	if current == nil || *current != admission.history {
		return errors.New("the repair changed release history; run yard migrate --check")
	}
	if admission.kind == admissionBounded || admission.request.handler == "@init" || admission.request.handler == "@teardown" {
		return nil // init verifies its own readiness; a removed yard has none
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, admission.request.yard)
	if err != nil || outcome == nil {
		return nil
	}
	if gate, ok := cli.yardScopedGate(outcome, inspection); ok {
		if state := gate.state(admission.request.yard); state.repairOnly() || state.pending {
			yard := admission.request.yard
			if state.materializedDrift && admission.sources == nil {
				_, gate.sourceRefusal = cli.repairSources(ctx)
			}
			cause, next := gate.yardAdvice(yard)
			if cause != nil {
				fmt.Fprintf(cli.options.Stderr, "yard: %v\n", cause)
			}
			fmt.Fprintf(cli.options.Stderr, "yard: yard %s still needs attention; next: %s\n", yard, next)
		}
	}
	return nil
}

// consumerRefreshAdmission decides the refresh of the yards a published
// configuration change reaches, on the release state after publication. A
// ready release runs the ordinary child. Otherwise the refresh is a repair of
// those consumer yards and carries what such a repair carries: a yard-scoped
// gate in which each consumer is open or limited to repair only by drift, the
// installed source bound, this inspection made under the owner barrier the
// sync holds, and release history unchanged by the refresh.
func (cli *CLI) consumerRefreshAdmission(ctx context.Context, yard string, names []string) (bool, func() error, error) {
	done := func() error { return nil }
	refuse := func(cause error, next string) (bool, func() error, error) {
		if next != "" {
			return false, nil, fmt.Errorf("configuration was published, but its refresh is refused: %w; next: %s", cause, next)
		}
		return false, nil, fmt.Errorf("configuration was published, but its refresh is refused: %w", cause)
	}
	if cli.ownerMutationLock == nil {
		return refuse(errors.New("owner barrier is not held"), "yard config sync pull --apply")
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, yard)
	if err != nil {
		return refuse(errors.New("release readiness could not be inspected"), repairSourceNext)
	}
	if outcome == nil {
		return false, done, nil
	}
	gate, ok := cli.yardScopedGate(outcome, inspection)
	if !ok {
		text := strings.TrimPrefix(mutationGateRefusalText(outcome, inspection), mutationGateRefusalPrefix+" ")
		return refuse(errors.New(strings.TrimSpace(strings.ReplaceAll(text, "\nyard: ", "; "))), "")
	}
	history, err := cli.readReleaseHistory()
	var historyErr error
	if err != nil || history == nil {
		historyErr = errors.New("protected release history could not be read")
	}
	sources, sourceErr := cli.repairSources(ctx)
	if cause, next := gate.configRefreshAdvice(names, domain.CollectRefusals(historyErr, sourceErr)); cause != nil {
		return refuse(cause, next)
	}
	admission := &gateAdmission{kind: admissionRepair, request: gateRequest{yard: yard, handler: "@config", name: "config"}, history: *history, sources: sources, rechecked: true}
	return true, func() error {
		for _, name := range names {
			admission.request.yard = name
			if err := admission.verifyAfterEffect(ctx, cli); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

// configRefreshAdvice uses the consumer gate's predicates for read-only advice.
func (gate yardGate) configRefreshAdvice(names []string, sourceErr error) (error, string) {
	var refusals []error
	var next, blockedNext string
	for _, name := range names {
		state := gate.state(name)
		for _, finding := range []struct {
			present bool
			reason  string
		}{{state.config, "invalid configuration"}, {state.failed, "failed activation"}, {state.unreachable, "unreachable guest"}, {state.pending, "pending configuration"}} {
			if finding.present {
				refusals = append(refusals, fmt.Errorf("yard %s has %s", name, finding.reason))
				if blockedNext == "" {
					blockedNext = yardNextStep(name, state)
				}
			}
		}
		if next == "" && state.materializedDrift {
			next = "yard -Y " + name + " config apply"
		}
	}
	switch {
	case sourceErr != nil:
		return domain.CollectRefusals(append(refusals, sourceErr)...), repairSourceNext
	case blockedNext != "":
		return domain.CollectRefusals(refusals...), blockedNext
	default:
		return nil, next
	}
}

func (gate yardGate) yardAdvice(yard string) (error, string) {
	state := gate.state(yard)
	if state.materializedDrift {
		return gate.configRefreshAdvice([]string{yard}, gate.sourceRefusal)
	}
	return nil, yardNextStep(yard, state)
}

// guardEffects refuses an adapter while a repair admission has not passed its
// recheck, so no repair reaches an effect outside preparedCommand.Execute.
func (cli *CLI) guardEffects(context.Context) error {
	if admission := cli.gateAdmitted; admission.repair() && !admission.rechecked {
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
		cause, next := gate.yardAdvice(name)
		if cause != nil {
			message += "; " + cause.Error()
		}
		findings = append(findings, releasetransition.Blocker{Code: releasetransition.CodeDependencyUnavailable, Message: message, Retry: next})
	}
	return findings
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
	gate, ok := cli.yardScopedGate(outcome, inspection)
	if ok && len(gate.materializedDriftYards()) != 0 {
		_, gate.sourceRefusal = cli.repairSources(ctx)
	}
	return gate, ok
}

func (gate yardGate) materializedDriftYards() []string {
	var names []string
	for _, name := range gate.order {
		if gate.state(name).materializedDrift {
			names = append(names, name)
		}
	}
	return names
}

func (cli *CLI) updateCheckAdvice(ctx context.Context, inspection *releasetransition.Inspection) (string, string) {
	gate, ok := cli.yardScopedGate(inspection.Outcome, inspection)
	names := gate.materializedDriftYards()
	if !ok || len(names) == 0 {
		return "", ""
	}
	_, sourceErr := cli.repairSources(ctx)
	cause, next := gate.configRefreshAdvice(names, sourceErr)
	if cause != nil {
		return cause.Error(), next
	}
	return "materialized configuration needs refresh in " + strings.Join(names, ", "), next
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
	cause, next := gate.yardAdvice(yard)
	if cause != nil {
		fmt.Fprintf(cli.options.Stdout, "  attention %s\n", cause)
	}
	fmt.Fprintf(cli.options.Stdout, "    next: %s\n", next)
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
