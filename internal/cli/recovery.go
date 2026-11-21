package cli

import (
	"cmp"
	"context"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
)

// The gate's recovery advice: the yard gate and the installed-source
// prerequisite are the observation, yardFindings types it, domain.NextStep
// selects one action that the gate admits and that is not the attempted
// command, and step renders it. No caller composes a command string.

// yardFindings are one yard's findings in stable order. For a yard that only
// a repair can fix, a refused installed source comes first: no repair runs
// until it is right.
func (gate yardGate) yardFindings(yard string) []domain.Finding {
	var findings []domain.Finding
	state := gate.state(yard)
	if gate.source != nil && state.repairOnly() {
		findings = append(findings, gate.source.finding())
	}
	add := func(on bool, kind domain.FindingKind, target string) {
		if on {
			findings = append(findings, domain.Finding{Kind: kind, Yard: target})
		}
	}
	configYard := yard
	if own := gate.yards[yard]; gate.everyYard && (own == nil || !own.config) {
		configYard = "default"
	}
	add(state.config, domain.FindingConfigInvalid, configYard)
	add(state.unreachable, domain.FindingUnreachable, yard)
	add(state.failed, domain.FindingFailed, yard)
	add(state.drift, domain.FindingDrift, yard)
	if state.pending {
		findings = append(findings, domain.Finding{Kind: domain.FindingPending, Yard: yard, Retry: state.retry})
	}
	if len(findings) == 0 {
		add(true, domain.FindingDrift, yard)
	}
	return findings
}

// actionSubject is the gate subject of a proposed yard action; host and read
// verbs are not gated by a yard.
func actionSubject(action domain.Action) (gateSubject, bool) {
	switch action.Verb {
	case domain.VerbInit:
		return initSubject(initArguments{mode: initReconcile}), true
	case domain.VerbConfigApply:
		// A refresh repairs file drift only: like a sync consumer refresh, it
		// is refused for a yard with any other finding.
		return gateSubject{declared: true, host: true, member: memberConfig, acts: []yardAction{{yard: cmp.Or(action.Yard, "default"), driftOnly: true, refusePending: true}}}, true
	case domain.VerbStart:
		return lifecycleSubject("start"), true
	case domain.VerbStopForce:
		return lifecycleSubject("stop"), true
	}
	return gateSubject{}, false
}

// admits is the gate's admission predicate for a proposed action.
func (gate yardGate) admits(action domain.Action) bool {
	subject, gated := actionSubject(action)
	if !gated {
		return true
	}
	yard := cmp.Or(action.Yard, "default")
	verdict := gate.verdict(yard, subject)
	return len(verdict.refusals) == 0 && (gate.source == nil || !gate.state(yard).repairOnly())
}

// step is the one next step of a yard behind this gate after attempted.
func (gate yardGate) step(yard string, attempted domain.Action) string {
	action, chosen := domain.NextStep(gate.yardFindings(yard), attempted, gate.admits)
	return domain.RenderNext(action, chosen, attempted)
}

// withSource records the installed-source prerequisite of release repair on a
// gate with a yard that only a repair can fix, so every entry point advises on
// the same observation.
func (cli *CLI) withSource(ctx context.Context, gate yardGate) yardGate {
	for _, name := range gate.order {
		if gate.state(name).repairOnly() {
			if _, err := cli.repairSources(ctx); err != nil {
				refusal, _ := err.(repairSourceRefusal)
				gate.source = &refusal
			}
			break
		}
	}
	return gate
}

// attempted is the recovery action an admitted invocation performs.
func (admission *gateAdmission) attempted() domain.Action {
	yard, arguments := admission.request.yard, admission.request.arguments
	switch admission.request.handler {
	case "@init":
		return domain.Action{Verb: domain.VerbInit, Yard: yard}
	case "@config":
		if verbs := configArguments(arguments); len(verbs) != 0 && verbs[0] == "apply" {
			return domain.Action{Verb: domain.VerbConfigApply, Yard: yard}
		}
	case "@current-migration":
		if !slices.Contains(arguments, "--check") {
			return domain.Action{Verb: domain.VerbMigrate}
		}
		return domain.Action{Verb: domain.VerbMigrateCheck}
	case "@lifecycle":
		if admission.request.name == "start" {
			return domain.Action{Verb: domain.VerbStart, Yard: yard}
		}
		if admission.request.name == "stop" && slices.Contains(arguments, "--force") {
			return domain.Action{Verb: domain.VerbStopForce, Yard: yard}
		}
	}
	return domain.Action{}
}
