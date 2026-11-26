package cli

import (
	"slices"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
)

// maxGateDecisions bounds the redacted decisions of a gate outcome in error data.
const maxGateDecisions = 32

// configRefusalData collects the typed configuration refusals of err through
// config.Diagnose, in its collected order. Only domain.ConfigRefusal values enter
// the data: their texts name validated names only. Source diagnostics need the
// config home to hide paths and stay with the owner's own diagnostics, so the
// home is not passed and any such cause marks the data incomplete.
func configRefusalData(err error) rpc.ErrorData {
	diagnostic := config.Diagnose("", err)
	data := rpc.ErrorData{Incomplete: !diagnostic.Complete()}
	for _, refusal := range diagnostic.Typed {
		data.Refusals = append(data.Refusals, rpc.Refusal{
			Source: rpc.RefusalSourceConfig, Code: refusal.Code.Name(),
			Text: refusal.Text, Next: refusal.Next,
		})
	}
	return data
}

// mutationGateRPCError carries every gate blocker as a typed refusal and the
// public outcome as gate data; message is the outcome's own message.
func mutationGateRPCError(outcome *releasetransition.Outcome, inspection *releasetransition.Inspection) *rpc.Error {
	public := publicInspectedMutationGateOutcome(outcome, inspection)
	refusals := []rpc.Refusal{}
	add := func(code releasetransition.OutcomeCode, resource, text, retry string) {
		refusal := gateRPCRefusal(code, resource, text, retry)
		if !slices.ContainsFunc(refusals, func(seen rpc.Refusal) bool {
			return seen.Code == refusal.Code && seen.Resource == refusal.Resource && seen.Text == refusal.Text && seen.Next == refusal.Next
		}) {
			refusals = append(refusals, refusal)
		}
	}
	for _, blocker := range public.Blockers {
		add(blocker.Code, blocker.Resource, blocker.Message, firstNonempty(blocker.Retry, public.Action))
	}
	if len(refusals) == 0 {
		add(public.Code, "", firstNonempty(public.Message, string(public.Code)), public.Action)
	}
	gate := public
	gate.Blockers = nil // They are the refusals.
	incomplete := len(gate.Decisions) > maxGateDecisions
	if incomplete {
		gate.Decisions = gate.Decisions[:maxGateDecisions]
	}
	return rpc.RefusalError(rpc.ErrorCode(outcome.Code), firstNonempty(outcome.Message, string(outcome.Code)),
		rpc.ErrorData{Refusals: refusals, Incomplete: incomplete, Gate: gate})
}

// gateRefusal reads an owner retry once: a parsed command becomes the typed
// action and its prose the next step; any other retry stays verbatim.
func gateRPCRefusal(code releasetransition.OutcomeCode, resource, text, retry string) rpc.Refusal {
	refusal := rpc.Refusal{Source: rpc.RefusalSourceGate, Code: string(code), Resource: resource, Text: firstNonempty(text, string(code)), Next: retry}
	if action, prose, ok := domain.ParseRetry(retry); ok {
		refusal.Next = prose
		refusal.Action = &rpc.RefusalAction{
			Verb: domain.VerbName(action.Verb), Yard: action.Yard, Version: action.Version,
			Command: domain.RenderAction(action),
		}
	}
	return refusal
}
