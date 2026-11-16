package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

// The yard-scoped release gate decides on facts the command itself prepared.
// Each prepare attaches a gateSubject from the request its handler already
// parsed and from its plan; one pure verdict decides it against a yard gate.
// The binding decision runs in preparedCommand.Execute after the owner barrier;
// an early verdict refuses before consent.

// repairMember is one member of the closed repair set of a yard that needs
// repair. The member is set from the handler's own parsed request.
type repairMember int

const (
	memberNone repairMember = iota
	memberInit
	memberInitReset
	memberTeardown
	memberStart
	memberStop
	memberShell
	memberConfig
	memberProfileDisable
	memberIntegrationDisable
	memberResourceShutdown
)

// repairMembers is the repair set in the order the refusal names it.
var repairMembers = []repairMember{memberInit, memberInitReset, memberTeardown, memberStart, memberStop, memberShell,
	memberConfig, memberProfileDisable, memberIntegrationDisable, memberResourceShutdown}

func (member repairMember) String() string {
	switch member {
	case memberInit:
		return "init"
	case memberInitReset:
		return "init --reset"
	case memberTeardown:
		return "teardown"
	case memberStart:
		return "start"
	case memberStop:
		return "stop"
	case memberShell:
		return "shell"
	case memberConfig:
		return "config (local authoring, apply, sync)"
	case memberProfileDisable:
		return "profile disable"
	case memberIntegrationDisable:
		return "integration disable"
	case memberResourceShutdown:
		return "resource shutdown"
	}
	return "none"
}

// yardScope is the set of yards whose desired configuration a command writes.
type yardScope int

const (
	scopeSelected yardScope = iota
	scopeNamed
	scopeEvery
)

// yardAction is another yard a command acts on. An optional action skips and
// reports a yard that cannot take it; a required one refuses the command.
type yardAction struct {
	yard     string
	optional bool
	// refusePending also refuses a required action on a yard with pending
	// findings: one that stops, restarts or reconfigures that yard.
	refusePending bool
}

// gateSubject is what the release gate knows about one prepared invocation.
// The zero value is undeclared: it writes every yard and is never a repair.
type gateSubject struct {
	declared bool
	// host commands act on the host; the selected yard does not gate them.
	host bool
	// member is the repair set member; unfit says why the invocation cannot be
	// that repair. Both are always computed, whatever the admission.
	member repairMember
	unfit  error
	// pending admits the invocation on a selected yard with only pending
	// findings: unrelated commands, plain init and profile resource verbs.
	pending bool
	// scope and named are the yards whose desired configuration it writes.
	scope yardScope
	named []string
	acts  []yardAction
}

// gateVerdict is the decision for one subject: every refusal in a stable
// order, the optional actions left out, and whether it repairs the selected
// yard.
type gateVerdict struct {
	refusals []error
	skip     []string
	repair   bool
}

func (verdict gateVerdict) err() error {
	if len(verdict.refusals) == 0 {
		return nil
	}
	return releaseGateRefusals(verdict.refusals)
}

// releaseGateRefusals joins refusals into one text error, one per line.
type releaseGateRefusals []error

func (refusals releaseGateRefusals) Error() string {
	lines := make([]string, 0, len(refusals))
	for _, refusal := range refusals {
		lines = append(lines, refusal.Error())
	}
	return strings.Join(lines, "\n")
}

func (refusals releaseGateRefusals) Unwrap() []error { return refusals }

// verdict decides one subject on the selected yard. It reads only the gate
// and the subject. Yards gate in a stable order: the selected yard, the other
// yards whose desired configuration it writes, then the yards it acts on;
// each yard refuses at most once.
func (gate yardGate) verdict(selected string, subject gateSubject) gateVerdict {
	var result gateVerdict
	if !subject.declared {
		subject = gateSubject{scope: scopeEvery}
	}
	refused := map[string]bool{}
	refuse := func(yard string, err error) {
		if !refused[yard] {
			refused[yard] = true
			result.refusals = append(result.refusals, err)
		}
	}
	if !subject.host {
		state := gate.state(selected)
		switch {
		case state.repairOnly() && subject.member == memberNone:
			refuse(selected, errors.New(yardGateRefusal(selected, gate)))
		case state.repairOnly() && subject.unfit != nil:
			refuse(selected, fmt.Errorf("%s\nthis invocation is not that repair: %v", yardGateRefusal(selected, gate), subject.unfit))
		case state.repairOnly():
			result.repair = true
		case state.pending && !subject.pending:
			refuse(selected, errors.New(yardGateRefusal(selected, gate)))
		}
	}
	var desired []string
	switch subject.scope {
	case scopeNamed:
		desired = slices.Clone(subject.named)
		slices.Sort(desired)
	case scopeEvery:
		desired = slices.Clone(gate.order)
	}
	configRepair := subject.member == memberConfig && subject.unfit == nil
	for _, yard := range desired {
		if yard == selected {
			continue
		}
		state := gate.state(yard)
		switch {
		case state.repairOnly() && !configRepair:
			refuse(yard, fmt.Errorf("release gate: this command writes the desired configuration of yard %s, which needs repair\nnext: %s", yard, yardNextStep(yard, state)))
		case state.pending:
			refuse(yard, fmt.Errorf("release gate: this command writes the desired configuration of yard %s, whose pending configuration refuses it\nnext: %s, then repeat this command",
				yard, yardNextStep(yard, state)))
		}
	}
	// A host command skipped the selected yard's check: each yard it acts on is
	// checked here, the selected one included.
	for _, action := range subject.acts {
		if action.yard == selected && !subject.host || refused[action.yard] || slices.Contains(result.skip, action.yard) {
			continue
		}
		state := gate.state(action.yard)
		switch {
		case action.optional && (state.repairOnly() || state.pending):
			result.skip = append(result.skip, action.yard)
		case action.optional:
		case state.repairOnly() && !configRepair:
			refuse(action.yard, fmt.Errorf("yard %s needs repair first; next: %s", action.yard, yardNextStep(action.yard, state)))
		case state.pending && action.refusePending:
			refuse(action.yard, fmt.Errorf("yard %s has pending configuration that this command would act on; next: %s", action.yard, yardNextStep(action.yard, state)))
		}
	}
	return result
}

// yardRepairSetText names the repair set in its order.
func yardRepairSetText() string {
	names := make([]string, 0, len(repairMembers))
	for _, member := range repairMembers {
		names = append(names, member.String())
	}
	return strings.Join(names, ", ")
}

// Subject builders, each from a handler's own parsed request.

func selectedSubject() gateSubject { return gateSubject{declared: true} }

func hostSubject() gateSubject { return gateSubject{declared: true, host: true, pending: true} }

// initSubject: plain init repeats the yard's whole convergence and --reset
// rebuilds it; --configs, --profile, --adopt and --continue-legacy change what
// the yard is or do only part of the work.
func initSubject(request initArguments) gateSubject {
	subject := selectedSubject()
	subject.member = memberInit
	if request.mode == initReset {
		subject.member = memberInitReset
	}
	switch {
	case request.adopt, request.continueLegacy, request.profile != "", request.mode != initReconcile && request.mode != initReset:
		subject.unfit = errors.New("only plain init and init --reset repair a yard; other init modes wait until it is converged")
	default:
		subject.pending = request.mode == initReconcile
	}
	return subject
}

func lifecycleSubject(name string) gateSubject {
	subject := selectedSubject()
	subject.pending = true
	switch name {
	case "start":
		subject.member = memberStart
	case "stop":
		subject.member = memberStop
	}
	return subject
}

func verbSubject(verb string, member repairMember) gateSubject {
	subject := selectedSubject()
	subject.member = member
	if verb != "disable" {
		subject.unfit = fmt.Errorf("%s is not a repair; only disable is", verb)
	}
	return subject
}

// configAuthoringSubject: local authoring at any scope is a configuration
// repair; Git authoring is not. A host or shared setting, and any Git write,
// reaches every yard's desired configuration.
func configAuthoringSubject(yard string, requests []configAuthoringRequest) gateSubject {
	subject := selectedSubject()
	subject.member = memberConfig
	for _, request := range requests {
		if request.git {
			subject.unfit = fmt.Errorf("Git authoring is not a repair; correct the setting by editing its file, then yard -Y %s config validate", firstNonempty(yard, "default"))
		}
		if request.git || request.scope != "yard" {
			subject.scope = scopeEvery
		}
	}
	return subject
}

// configSyncSubject writes the yards its plan changes; a change it cannot
// place in one yard reaches every yard. Its consumer refreshes are required
// actions on their yards.
func configSyncSubject(paths []string, consumers []string) gateSubject {
	subject := selectedSubject()
	subject.member = memberConfig
	subject.scope = scopeNamed
	for _, path := range paths {
		yard, ok := configSyncPathYard(path)
		if !ok {
			subject.scope, subject.named = scopeEvery, nil
			break
		}
		if !slices.Contains(subject.named, yard) {
			subject.named = append(subject.named, yard)
		}
	}
	for _, consumer := range consumers {
		subject.acts = append(subject.acts, yardAction{yard: consumer, refusePending: true})
	}
	return subject
}

// handlerSubjects are the declared subjects of prepared handlers whose
// subject handler identity decides. Handlers that set their own subject in
// prepare are not listed; any other handler is undeclared.
var handlerSubjects = map[string]gateSubject{
	"@host":              hostSubject(),
	"@keys":              hostSubject(),
	"@test-vms":          hostSubject(),
	"@network":           hostSubject(),
	"@client-projects":   hostSubject(),
	"@cleanup":           {declared: true, pending: true},
	"@provision":         {declared: true},
	"@service":           {declared: true, pending: true},
	"@project":           {declared: true, pending: true},
	"@project-env":       {declared: true, pending: true},
	"@remote":            {declared: true, pending: true},
	"@update":            {declared: true},
	"@current-migration": {declared: true},
}

// nonPreparedMutators are the mutating handlers that run without a prepared
// command. Handler identity decides them before dispatch, which is their only
// gate check.
var nonPreparedMutators = map[string]gateSubject{
	"@shell":           {declared: true, member: memberShell, pending: true},
	"@ssh-agent":       {declared: true, pending: true},
	"@authorize":       {declared: true, pending: true},
	"@state":           {declared: true, pending: true},
	"@project-state":   hostSubject(),
	"@vm-start-policy": hostSubject(),
}

// gateDispatchedElsewhere are mutating handlers without a prepared command
// that this gate does not decide; each names who does.
var gateDispatchedElsewhere = map[string]string{
	"@rpc":                "every operation passes its own operation admission",
	"@migrate":            "the release migration owns its release admission",
	"@release-transition": "the release transition engine owns its release admission",
	"@resource":           "svc resolves to its resource invocation, which is admitted as that resource",
}

// planHasEffect reports a plan that may change state: a step it applies, a
// changed assessment with any effect but read, or a mutating plan that asks
// for consent with neither an assessment nor steps to show otherwise.
func planHasEffect(plan domain.OperationPlan) bool {
	for _, step := range plan.Steps {
		if step.Decision == domain.StepApply || step.Decision == domain.StepConditional {
			return true
		}
	}
	if plan.Assessment == nil {
		// Without an assessment or steps, a mutating plan that asks for
		// consent cannot show it is a reader.
		return len(plan.Steps) == 0 && plan.Effect != domain.CommandRead && plan.Confirmation != domain.ConfirmationNever
	}
	return plan.Assessment.Changed && plan.Assessment.Effect != domain.ActionRead
}

// Gate state of a prepared command. Only these methods read its admission.

// gateAdmission is the admission this command runs under, taken once from the
// invocation's pre-dispatch admission. Nil is a command no gate admitted, such
// as an internal one built by a test or a handler outside any invocation.
func (prepared *preparedCommand) gateAdmission() *gateAdmission {
	if prepared.gateRecord == nil && prepared.CLI != nil {
		prepared.gateRecord = prepared.CLI.gateAdmitted
	}
	return prepared.gateRecord
}

// setGateSubject records what this command writes and acts on.
func (prepared *preparedCommand) setGateSubject(subject gateSubject) {
	prepared.gateDeclared = &subject
}

// gateSubject is the declared subject, by handler identity when its prepare
// set none, and undeclared otherwise.
func (prepared *preparedCommand) gateSubject() gateSubject {
	if prepared.gateDeclared != nil {
		return *prepared.gateDeclared
	}
	if subject, ok := handlerSubjects[prepared.Definition.Handler]; ok {
		return subject
	}
	return gateSubject{}
}

// gateVerdictNow decides the subject on the admission's snapshot.
func (prepared *preparedCommand) gateVerdictNow() (gateVerdict, bool) {
	admission := prepared.gateAdmission()
	if admission == nil || admission.gate == nil || admission.kind != admissionOpen && admission.kind != admissionRepair {
		return gateVerdict{}, false
	}
	return admission.gate.verdict(admission.request.yard, prepared.gateSubject()), true
}

// earlyGate refuses before consent what Execute would refuse, on the
// admission's snapshot, and records the optional actions it leaves out.
func (prepared *preparedCommand) earlyGate() error {
	verdict, decided := prepared.gateVerdictNow()
	if !decided {
		return nil
	}
	prepared.gateSkip = verdict.skip
	return verdict.err()
}

// gateSkipped leaves out, and reports once, the targets the admission's
// snapshot does not let this command act on.
func (prepared *preparedCommand) gateSkipped(targets []configTarget, report bool) []configTarget {
	verdict, decided := prepared.gateVerdictNow()
	if !decided || len(verdict.skip) == 0 {
		return targets
	}
	admission := prepared.gateRecord
	return slices.DeleteFunc(slices.Clone(targets), func(target configTarget) bool {
		if !slices.Contains(verdict.skip, target.Name) {
			return false
		}
		if report {
			fmt.Fprintf(prepared.CLI.options.Stderr, "yard: skipped yard %s: it needs attention; next: %s\n", target.Name, yardNextStep(target.Name, admission.gate.state(target.Name)))
		}
		return true
	})
}

// bindingCurrent requires the release history and installed source a repair
// admission was made on; under the owner barrier no writer can change them.
func (admission *gateAdmission) bindingCurrent(ctx context.Context, cli *CLI) error {
	stale := fmt.Errorf("%w: the release changed after the plan; plan the command again", domain.ErrPlanStale)
	current, err := cli.readReleaseHistory()
	if err != nil || current == nil || *current != admission.history {
		return stale
	}
	if admission.sources != nil {
		sources, err := cli.repairSources(ctx)
		if err != nil || *sources != *admission.sources {
			return stale
		}
	}
	return nil
}

// gateBindObserver, set only by tests, sees every prepared command that
// reaches its binding gate decision.
var gateBindObserver func(*preparedCommand)

// bindGate is the binding gate decision, in Execute after the owner barrier.
// An open admission decides its subject on the snapshot it was admitted on,
// without a new inspection. A repair is rechecked against fresh release state
// and its subject decided on that state; a bounded stop admits only stop.
func (prepared *preparedCommand) bindGate(ctx context.Context) error {
	if gateBindObserver != nil {
		gateBindObserver(prepared)
	}
	if planned := prepared.gatePlanned; planned != nil {
		if err := planned.bindingCurrent(ctx, prepared.CLI); err != nil {
			return err
		}
	}
	admission := prepared.gateAdmission()
	if admission == nil {
		// Every invocation and RPC operation carries an explicit kind; only a
		// test builds a prepared command outside them.
		if testing.Testing() {
			return nil
		}
		return errors.New("internal error: a prepared command reached execution without a release gate admission")
	}
	subject := prepared.gateSubject()
	switch admission.kind {
	case admissionOpen:
		verdict := admission.gate.verdict(admission.request.yard, subject)
		if err := verdict.err(); err != nil {
			return err
		}
		if verdict.repair {
			return fmt.Errorf("%w: yard %s needs repair since its admission; rerun the command", domain.ErrPlanStale, admission.request.yard)
		}
	case admissionRepair:
		if err := admission.recheck(ctx, prepared.CLI); err != nil {
			return err
		}
		verdict := admission.gate.verdict(admission.request.yard, subject)
		if err := verdict.err(); err != nil {
			return err
		}
		if !slices.Equal(verdict.skip, prepared.gateSkip) {
			return fmt.Errorf("%w: the yards this command acts on changed after confirmation; rerun the command", domain.ErrPlanStale)
		}
		prepared.repairing = verdict.repair
	case admissionBounded:
		if subject.member != memberStop {
			return errors.New("over an unfinished activation only yard stop runs")
		}
		if err := admission.recheck(ctx, prepared.CLI); err != nil {
			return err
		}
	}
	return nil
}
