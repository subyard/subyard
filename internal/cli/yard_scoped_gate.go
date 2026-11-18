package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/shellquote"
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
	yards    map[string]*yardGateState
	order    []string
	blockers []releasetransition.Blocker
	message  string
	// everyYard is set when the default yard's own configuration is invalid:
	// it underlies every yard, so each yard admits only its repair commands.
	everyYard bool
	advice    func(string) yardRepairAdvice
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
	gate.blockers = inspection.Blockers
	gate.message = outcome.Message
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

// yardRecovery carries one executable command and the reason it takes precedence.
type yardRecovery struct {
	prerequisite string
	command      string
}

func yardNextStep(yard string, state yardGateState) yardRecovery {
	switch {
	case state.config:
		return yardRecovery{command: "yard -Y " + yard + " config validate"}
	case state.unreachable:
		return yardRecovery{command: "yard migrate"}
	case !state.failed && !state.drift && state.retry != "":
		return yardRecovery{command: strings.TrimPrefix(state.retry, "run ")}
	}
	return yardRecovery{command: "yard -Y " + yard + " init"}
}

type yardGateRefusal struct {
	text string
	next string
}

func (refusal yardGateRefusal) Error() string { return refusal.text }

// selectedYardRefusal reports a yard that admits only repair.
func (gate yardGate) selectedYardRefusal(yard string) error {
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
	}
	advice := gate.adviceFor(yard)
	return yardGateRefusal{
		text: fmt.Sprintf("release gate: yard %s needs repair: %s\n%s\nuntil then only its repair commands run: %s",
			yard, cause, advice.hint(), yardRepairSetText()),
		next: advice.recovery.command,
	}
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
	gate = cli.withYardAdvice(ctx, gate)
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
	// A shell into a yard that needs repair diagnoses it with any tooling; its
	// history is still recorded.
	var after *releaseHistoryToken
	var afterErr error
	if admitted.kind == admissionRepair && request.handler != "@shell" {
		after, admitted.sources, afterErr = cli.repairPrerequisites(ctx)
	} else {
		after, afterErr = cli.readReleaseHistory()
		if afterErr != nil {
			afterErr = errRepairHistoryUnreadable
		}
	}
	if beforeErr != nil {
		beforeErr = errRepairHistoryUnreadable
	}
	if err = domain.CollectRefusals(beforeErr, afterErr); err != nil {
		return nil, outcome, inspection, nil, scoped, cli.repairSourceRefusal(admitted, err)
	}
	if before == nil || after == nil || *before != *after {
		return nil, outcome, inspection, nil, scoped, fmt.Errorf("%w: the release changed during admission; rerun the command", domain.ErrPlanStale)
	}
	admitted.history = *after
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
	ledger, ledgerErr := store.ReadLedger()
	checkpoint, checkpointErr := store.ReadMigrationCheckpoint()
	if err := domain.CollectRefusals(err, ledgerErr, checkpointErr); err != nil {
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

var (
	errRepairHistoryUnreadable = errors.New("protected release history could not be read")
	errRepairToolingMismatch   = errors.New("running tooling does not match the sealed completed release")
)

// repairPrerequisites is the installed source and history check shared by
// repair admission, consumer refresh, and the advice for those operations.
func (cli *CLI) repairPrerequisites(ctx context.Context) (*releaseHistoryToken, *repairSourceBinding, error) {
	history, historyErr := cli.readReleaseHistory()
	if historyErr != nil || history == nil {
		historyErr = errRepairHistoryUnreadable
	}
	sources, sourceErr := cli.repairSources(ctx)
	return history, sources, domain.CollectRefusals(historyErr, sourceErr)
}

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
		if cli.sourceBinding != nil {
			err = errors.New(releaseruntime.SourceAdmissionPredicate(err))
		}
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
		refusals = append(refusals, errRepairToolingMismatch)
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
		return cli.repairSourceRefusal(admission, errRepairHistoryUnreadable)
	}
	if current == nil || *current != admission.history {
		return fmt.Errorf("%w: the release changed after confirmation; rerun the command", domain.ErrPlanStale)
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, admission.request.yard)
	if err != nil {
		return err
	}
	if outcome != nil {
		again, gate, refusal, _ := cli.decideGate(ctx, admission.request, outcome, inspection)
		if again == nil {
			if refusal != nil {
				return fmt.Errorf("%w: the release changed after confirmation: %w", domain.ErrPlanStale, refusal)
			}
			return fmt.Errorf("%w: the release changed after confirmation: %s", domain.ErrPlanStale, strings.TrimSpace(mutationGateRefusalText(outcome, inspection)))
		}
		if again.kind == admissionBounded {
			gate = yardGate{}
		}
		admission.gate = &gate
	} else {
		admission.gate = &yardGate{}
	}
	if admission.sources != nil {
		_, sources, err := cli.repairPrerequisites(ctx)
		if err != nil {
			return cli.repairSourceRefusal(admission, err)
		}
		if *sources != *admission.sources {
			return fmt.Errorf("%w: the installed release source changed after confirmation; rerun the command", domain.ErrPlanStale)
		}
	}
	if current, err = cli.readReleaseHistory(); err != nil {
		return cli.repairSourceRefusal(admission, errRepairHistoryUnreadable)
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
		return fmt.Errorf("%w; next: yard migrate --check", errRepairHistoryUnreadable)
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
			advice := cli.withYardAdvice(ctx, gate).adviceFor(yard)
			fmt.Fprintf(cli.options.Stderr, "yard: yard %s still needs attention; %s\n", yard, advice.hint())
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
		return refuse(errors.New("owner barrier is not held"), "retry the same sync on the owner host")
	}
	outcome, inspection, err := cli.inspectMutationGateInspection(ctx, yard)
	if err != nil {
		return refuse(errors.New("release readiness could not be inspected"), "yard migrate --check")
	}
	if outcome == nil {
		return false, done, nil
	}
	gate, ok := cli.yardScopedGate(outcome, inspection)
	if !ok {
		text := strings.Replace(mutationGateRefusalText(outcome, inspection), "; this command is not run", "", 1)
		text = strings.TrimPrefix(text, mutationGateRefusalPrefix+" ")
		return refuse(errors.New(strings.TrimSpace(strings.ReplaceAll(text, "\nyard: ", "; "))), "")
	}
	history, sources, sourceErr := cli.repairPrerequisites(ctx)
	if blocked, cause := gate.configRefreshRefusal(names); cause != nil || sourceErr != nil {
		if sourceErr != nil {
			retry := "yard -Y " + yard + " config status"
			if blocked == "" && len(names) != 0 {
				retry = "yard -Y " + names[0] + " config apply"
			}
			recovery := cli.sourceRecovery(&gate, sourceErr, retry)
			return refuse(domain.CollectRefusals(cause, errors.New(recovery.prerequisite)), recovery.command)
		}
		return refuse(cause, cli.withYardAdvice(ctx, gate).adviceFor(blocked).recovery.command)
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

// sourceRecovery uses the installed launcher when tooling is the only failed
// prerequisite. Other source failures can require inspecting protected state
// first, since update itself may be blocked by a yard or unfinished activation.
func (cli *CLI) sourceRecovery(gate *yardGate, err error, retry string) yardRecovery {
	recovery := yardRecovery{prerequisite: err.Error(), command: "yard migrate --check"}
	if err == errRepairToolingMismatch {
		options, available, optionsErr := cli.mutationGateReleaseOptions()
		if args, ok := strings.CutPrefix(retry, "yard "); ok && optionsErr == nil && available {
			recovery.command = shellquote.Word(filepath.Join(options.RuntimeRoot, "current", "bin", "yard")) + " " + args
		}
	} else if gate != nil && !errors.Is(err, errRepairHistoryUnreadable) {
		if _, blocker := gate.configRefreshRefusal(gate.order); blocker == nil {
			recovery.command = "yard update"
		}
	}
	return recovery
}

func (cli *CLI) repairSourceRefusal(admission *gateAdmission, err error) error {
	request := admission.request
	recovery := cli.sourceRecovery(admission.gate, err, request.repairRetry())
	var findings []error
	if admission.gate != nil {
		for _, message := range admission.gate.state(request.yard).messages {
			findings = append(findings, errors.New(message))
		}
	}
	findings = append(findings, errors.New(recovery.prerequisite))
	return fmt.Errorf("%w: %w; next: %s", domain.ErrPlanStale, domain.CollectOrderedRefusals(findings...), recovery.command)
}

// Repair arguments may include setting values or paths. Only echo a command
// whose arguments are known public flags.
func (request gateRequest) repairRetry() string {
	if !domain.SafeName(request.yard) {
		return "yard migrate --check"
	}
	name, args := request.name, request.arguments
	if name == "config" && len(args) != 0 && args[0] == "apply" {
		name, args = "config apply", args[1:]
	}
	if slices.Contains([]string{"init", "teardown", "stop", "start", "config apply"}, name) &&
		!slices.ContainsFunc(args, func(arg string) bool {
			return !slices.Contains([]string{"--yes", "-y", "--force", "--configs", "--reset", "--all-local"}, arg)
		}) {
		return strings.Join(append([]string{"yard", "-Y", request.yard, name}, args...), " ")
	}
	return "yard -Y " + request.yard + " config status"
}

// configRefreshRefusal is shared by consumer admission and repair advice.
func (gate yardGate) configRefreshRefusal(names []string) (string, error) {
	var refusals []error
	var blocked string
	for _, name := range names {
		state := gate.state(name)
		for _, finding := range []struct {
			present bool
			reason  string
		}{{state.config, "invalid configuration"}, {state.failed, "failed activation"}, {state.unreachable, "unreachable guest"}, {state.pending, "pending configuration"}} {
			if finding.present {
				refusals = append(refusals, fmt.Errorf("yard %s has %s", name, finding.reason))
				if blocked == "" {
					blocked = name
				}
			}
		}
	}
	return blocked, domain.CollectRefusals(refusals...)
}

// fileRefreshNeeded publishes only an activation diagnostic from a failed probe.
func (cli *CLI) fileRefreshNeeded(ctx context.Context, yard string) (changed bool, diagnostic string) {
	operation := *cli
	operation.env = cli.baseEnv
	loaded, probeFailure := operation.resolveContextWithYardSettings(yard, "")
	if probeFailure == nil {
		assessment, err := operation.prepareConfigApply(ctx, []configTarget{{Name: yard, Loaded: loaded}}, nil)
		if err == nil {
			return assessment.changed(), ""
		}
		probeFailure = err
	}
	home := loaded.Context.Paths.ConfigHome
	if home == "" {
		home, _ = config.ResolveConfigHome(firstNonempty(cli.env["SUBYARD_OPERATOR_HOME"], cli.env["HOME"]), cli.env)
	}
	finding := materializedConfigActivationError{probeFailure, yard, "materialized config", "config status", "inspect", home}
	public := activationRefusals{causes: finding.appendTo(nil)}
	diagnostic, _ = public.ActivationDiagnostic()
	return false, diagnostic
}

type yardRepairAdvice struct {
	diagnostic string
	recovery   yardRecovery
}

// Advice uses the same subject verdict and installed-source prerequisites as
// the command it proposes. Keep the undecorated gate for testing candidates.
func (cli *CLI) withYardAdvice(ctx context.Context, gate yardGate) yardGate {
	checkSource := sync.OnceValue(func() error {
		_, _, err := cli.repairPrerequisites(ctx)
		return err
	})
	decisions := gate
	decisions.advice = nil
	gate.advice = func(yard string) yardRepairAdvice {
		state := decisions.state(yard)
		advice := yardRepairAdvice{recovery: yardNextStep(yard, state)}
		init := decisions.verdict(yard, initSubject(initArguments{mode: initReconcile}))
		if init.repair {
			if err := checkSource(); err != nil {
				advice.recovery = cli.sourceRecovery(&decisions, err, advice.recovery.command)
				return advice
			}
		}
		if state.materializedDrift {
			_, refusal := decisions.configRefreshRefusal([]string{yard})
			if refusal == nil {
				changed, diagnostic := cli.fileRefreshNeeded(ctx, yard)
				switch {
				case diagnostic != "":
					advice.diagnostic = diagnostic
					advice.recovery.command = "yard -Y " + yard + " config status"
				case changed:
					advice.recovery.command = "yard -Y " + yard + " config apply"
				}
			}
		}
		return advice
	}
	return gate
}

func (gate yardGate) adviceFor(yard string) yardRepairAdvice {
	if gate.advice != nil {
		return gate.advice(yard)
	}
	return yardRepairAdvice{recovery: yardNextStep(yard, gate.state(yard))}
}

func (gate yardGate) repairHint(yard string) string {
	return gate.adviceFor(yard).hint()
}

func (advice yardRepairAdvice) hint() string {
	text := ""
	if advice.diagnostic != "" {
		text += advice.diagnostic + "\n"
	}
	if advice.recovery.prerequisite != "" {
		text += "prerequisite: " + advice.recovery.prerequisite + "\n"
	}
	return text + "next: " + advice.recovery.command
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
func (cli *CLI) otherYardFindings(ctx context.Context, gate yardGate, yard string) []releasetransition.Blocker {
	gate = cli.withYardAdvice(ctx, gate)
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
		advice := gate.adviceFor(name)
		for _, detail := range []string{advice.diagnostic, advice.recovery.prerequisite} {
			if detail != "" {
				message += "; " + detail
			}
		}
		findings = append(findings, releasetransition.Blocker{Code: releasetransition.CodeDependencyUnavailable, Message: message, Retry: advice.recovery.command})
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
	return cli.withYardAdvice(ctx, gate), ok
}

func (cli *CLI) updateAdvice(ctx context.Context, inspection *releasetransition.Inspection) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, profile.ReadinessTimeout)
	defer cancel()
	gate, ok := cli.yardScopedGate(inspection.Outcome, inspection)
	if !ok || len(gate.order) == 0 {
		return "", ""
	}
	return cli.withYardAdvice(ctx, gate).releaseAdvice()
}

func (gate yardGate) releaseAdvice() (string, string) {
	yard, refusal := gate.configRefreshRefusal(gate.order)
	if yard == "" {
		yard = gate.order[0]
		for _, name := range gate.order {
			if gate.state(name).materializedDrift {
				yard = name
				break
			}
		}
	}
	var messages []string
	for _, blocker := range gate.blockers {
		if !slices.Contains(messages, blocker.Message) {
			messages = append(messages, blocker.Message)
		}
	}
	if len(messages) == 0 && refusal != nil {
		messages = append(messages, refusal.Error())
	}
	if len(messages) == 0 && gate.message != "" {
		messages = append(messages, gate.message)
	}
	if len(messages) == 0 {
		messages = append(messages, "activation needs attention in "+strings.Join(gate.order, ", "))
	}
	advice := gate.adviceFor(yard)
	for _, detail := range []string{advice.diagnostic, advice.recovery.prerequisite} {
		if detail != "" && !slices.Contains(messages, detail) {
			messages = append(messages, detail)
		}
	}
	return strings.Join(messages, "; "), advice.recovery.command
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
	if _, implicated := gate.yards[yard]; !implicated && !gate.everyYard {
		return
	}
	if gate.everyYard && yard != "default" {
		fmt.Fprintln(cli.options.Stdout, "  attention the default yard's configuration is invalid; it underlies this yard")
		yard = "default"
	} else {
		messages := gate.state(yard).messages
		if len(messages) == 0 {
			messages = []string{"this yard needs attention"}
		}
		for _, message := range messages {
			fmt.Fprintf(cli.options.Stdout, "  attention %s\n", message)
		}
	}
	advice := gate.adviceFor(yard)
	for _, detail := range []string{advice.diagnostic, advice.recovery.prerequisite} {
		if detail != "" {
			fmt.Fprintf(cli.options.Stdout, "  attention %s\n", detail)
		}
	}
	fmt.Fprintf(cli.options.Stdout, "    next: %s\n", advice.recovery.command)
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
