package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/rpc"
)

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
	return rpc.RefusalError(rpc.ErrorCode(outcome.Code), firstNonempty(outcome.Message, string(outcome.Code)),
		rpc.ErrorData{Refusals: refusals, Gate: rpcGateOutcome(public)})
}

// rpcGateOutcome is the public outcome without its blockers, which are the
// refusals; rpc.RefusalError bounds it.
func rpcGateOutcome(public mutationGateOutcome) *rpc.GateOutcome {
	gate := &rpc.GateOutcome{
		Status: string(public.Status), Code: string(public.Code),
		Active: string(public.Active), Target: string(public.Target),
		Message: public.Message, Action: public.Action,
	}
	if public.Previous != nil {
		previous := string(*public.Previous)
		gate.Previous = &previous
	}
	if public.Transaction != nil {
		transaction := string(*public.Transaction)
		gate.Transaction = &transaction
	}
	for _, decision := range public.Decisions {
		gate.Decisions = append(gate.Decisions, rpc.GateDecision{
			Resource: decision.Resource, Scope: decision.Scope,
			Decision: string(decision.Decision), Result: decision.Result,
		})
	}
	return gate
}

// gateRPCRefusal reads an owner retry once: a parsed command becomes the typed
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

// ownerErrorReport renders an error carrying an owner RPC error for local
// stderr. The owner's code and message pass through rpc.BoundText; with valid
// typed data the message, which repeats the refusals, becomes a fixed summary
// and the refusal lines follow it.
func ownerErrorReport(err error) (string, []string, bool) {
	var fault *rpc.Error
	if !errors.As(err, &fault) {
		return "", nil, false
	}
	summary := rpc.BoundText(fault.Message, rpc.MaxRefusalText)
	lines := ownerRefusalLines(fault)
	if data, ok := rpc.DecodeErrorData(fault); ok {
		summary = "the owner refused this request"
		if data.Gate != nil {
			summary = "the owner's release gate refused this command"
		}
	}
	owner := rpc.BoundText(string(fault.Code), rpc.MaxRefusalName) + ": " + summary
	text := err.Error()
	if raw := fault.Error(); strings.Contains(text, raw) {
		return strings.Replace(text, raw, owner, 1), lines, true
	}
	// A wrapper that reworded the owner text still prints no raw peer text.
	return rpc.BoundText(text, len(text)), lines, true
}

// ownerRefusalLines renders the error data of an owner RPC error: the gate
// state, then every refusal with its next step in the owner's order, then what
// the owner left out. rpc.DecodeErrorData bounds and cleans every text.
func ownerRefusalLines(err error) []string {
	var fault *rpc.Error
	if !errors.As(err, &fault) {
		return nil
	}
	data, ok := rpc.DecodeErrorData(fault)
	if !ok {
		return nil
	}
	lines := []string{}
	if gate := data.Gate; gate != nil {
		transaction := ""
		if gate.Transaction != nil {
			transaction = ", transaction " + *gate.Transaction
		}
		lines = append(lines, fmt.Sprintf("owner release gate: release %s (%s): active %s, target %s%s",
			gate.Status, gate.Code, firstNonempty(gate.Active, "unknown"), firstNonempty(gate.Target, "unknown"), transaction))
	}
	for _, refusal := range data.Refusals {
		lines = append(lines, "cause: "+firstNonempty(refusal.Text, refusal.Code))
		next := refusal.Next
		if refusal.Action != nil && refusal.Action.Command != "" {
			if next != "" {
				next += ", then "
			}
			next += "run " + refusal.Action.Command
		}
		if next != "" {
			lines = append(lines, "Next: "+next)
		}
	}
	if data.Omitted != 0 {
		lines = append(lines, fmt.Sprintf("%d more refusals; review the owner's diagnostics", data.Omitted))
	}
	if data.Incomplete {
		lines = append(lines, "other causes are reported only in the owner's diagnostics")
	}
	return lines
}
