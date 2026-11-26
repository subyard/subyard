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

// maxGateMessage bounds the plain-text message of a gate refusal.
const maxGateMessage = 2048

// mutationGateRPCError carries every gate blocker as a typed refusal and the
// public outcome as gate data. Its message stays self-contained for clients
// and relays that do not read data: see gateRPCMessage.
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
	return rpc.RefusalError(rpc.ErrorCode(outcome.Code), gateRPCMessage(public),
		rpc.ErrorData{Refusals: refusals, Gate: rpcGateOutcome(public)})
}

// gateRPCMessage renders the gate refusal as one bounded plain-text line, in the
// style of mutationGateRefusalText: the outcome message and release state, the
// outcome's next step unless a blocker carries it, then every distinct cause with
// its resource and retry. The next step precedes the causes so the bound cuts
// causes first.
func gateRPCMessage(public mutationGateOutcome) string {
	var text strings.Builder
	transaction := ""
	if public.Transaction != nil {
		transaction = ", transaction " + string(*public.Transaction)
	}
	fmt.Fprintf(&text, "%s: release %s (%s): active %s, target %s%s",
		firstNonempty(public.Message, string(public.Code)), public.Status, public.Code,
		firstNonempty(string(public.Active), "unknown"), firstNonempty(string(public.Target), "unknown"), transaction)
	if public.Action != "" && !slices.ContainsFunc(public.Blockers, func(blocker releasetransition.Blocker) bool {
		return blocker.Retry == public.Action
	}) {
		text.WriteString("; next: " + public.Action)
	}
	seen := []string{}
	for _, blocker := range public.Blockers {
		cause := "cause: " + firstNonempty(blocker.Message, string(blocker.Code))
		if blocker.Resource != "" {
			cause += " (" + blocker.Resource + ")"
		}
		if blocker.Retry != "" {
			cause += ", next: " + blocker.Retry
		}
		if !slices.Contains(seen, cause) {
			seen = append(seen, cause)
			text.WriteString("; " + cause)
		}
	}
	return rpc.BoundText(text.String(), maxGateMessage)
}

// rpcGateOutcome is the rendered part of the public outcome; its blockers are
// the refusals. rpc.RefusalError bounds it.
func rpcGateOutcome(public mutationGateOutcome) *rpc.GateOutcome {
	gate := &rpc.GateOutcome{
		Status: string(public.Status), Code: string(public.Code),
		Active: string(public.Active), Target: string(public.Target),
		Message: public.Message, Action: public.Action,
	}
	if public.Transaction != nil {
		transaction := string(*public.Transaction)
		gate.Transaction = &transaction
	}
	return gate
}

// gateRPCRefusal reads an owner retry once: a parsed command becomes the typed
// action and its prose the next step; any other retry stays verbatim.
func gateRPCRefusal(code releasetransition.OutcomeCode, resource, text, retry string) rpc.Refusal {
	refusal := rpc.Refusal{Source: rpc.RefusalSourceGate, Code: string(code), Resource: resource, Text: firstNonempty(text, string(code)), Next: retry}
	if action, prose, ok := domain.ParseRetry(retry); ok {
		refusal.Next = prose
		refusal.Action = &rpc.RefusalAction{Verb: domain.VerbName(action.Verb), Command: domain.RenderAction(action)}
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
	// The longest message an owner sends is a self-contained gate refusal.
	summary := rpc.BoundText(fault.Message, maxGateMessage)
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
// state and message, then every refusal with its code, resource and next step
// in the owner's order, then the gate's own next step and what the owner left
// out. rpc.DecodeErrorData bounds and cleans every text.
func ownerRefusalLines(err error) []string {
	var fault *rpc.Error
	if !errors.As(err, &fault) {
		return nil
	}
	data, ok := rpc.DecodeErrorData(fault)
	if !ok {
		return nil
	}
	lines, texts, nexts := []string{}, []string{}, []string{}
	for _, refusal := range data.Refusals {
		texts = append(texts, refusal.Text)
	}
	if gate := data.Gate; gate != nil {
		transaction := ""
		if gate.Transaction != nil {
			transaction = ", transaction " + *gate.Transaction
		}
		lines = append(lines, fmt.Sprintf("owner release gate: release %s (%s): active %s, target %s%s",
			gate.Status, gate.Code, firstNonempty(gate.Active, "unknown"), firstNonempty(gate.Target, "unknown"), transaction))
		if gate.Message != "" && !slices.Contains(texts, gate.Message) {
			lines = append(lines, "owner release gate: "+gate.Message)
		}
	}
	for _, refusal := range data.Refusals {
		cause := "cause: " + refusal.Text
		if refusal.Source == rpc.RefusalSourceGate {
			cause += " (" + refusal.Code
			if refusal.Resource != "" {
				cause += ", " + refusal.Resource
			}
			cause += ")"
		}
		lines = append(lines, cause)
		next := refusal.Next
		if refusal.Action != nil {
			if next != "" {
				next += ", then "
			}
			next += "run " + refusal.Action.Command
		}
		if next != "" {
			lines = append(lines, "Next: "+next)
			nexts = append(nexts, next)
		}
	}
	if gate := data.Gate; gate != nil && gate.Action != "" && !slices.Contains(nexts, gate.Action) {
		lines = append(lines, "Next: "+gate.Action)
	}
	if data.Omitted != 0 {
		lines = append(lines, fmt.Sprintf("%d more refusals; review the owner's diagnostics", data.Omitted))
	}
	if data.Incomplete {
		lines = append(lines, "other causes are reported only in the owner's diagnostics")
	}
	return lines
}
