package rpc

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds of ErrorData. Clients reject data outside them.
const (
	MaxErrorRefusals    = 32
	MaxRefusalText      = 512
	MaxRefusalName      = 128
	MaxGateDecisions    = 32
	errorDataSchema     = 1
	RefusalSourceConfig = "config"
	RefusalSourceGate   = "release-gate"
)

// ErrorData is the typed diagnostic attached to an RPC error. It carries only
// value-free refusals that the owner already renders to its operator.
type ErrorData struct {
	SchemaVersion int       `json:"schemaVersion"`
	Refusals      []Refusal `json:"refusals"`
	// Omitted counts refusals beyond MaxErrorRefusals.
	Omitted int `json:"omitted,omitempty"`
	// Incomplete means some causes are not typed refusals; the owner's own
	// diagnostics name them.
	Incomplete bool `json:"incomplete,omitempty"`
	// Gate is the public release-gate outcome of a gate refusal.
	Gate *GateOutcome `json:"gate,omitempty"`
}

// GateOutcome is the public release-gate outcome of a gate refusal; its
// blockers are the refusals.
type GateOutcome struct {
	Status      string         `json:"status"`
	Code        string         `json:"code"`
	Active      string         `json:"active"`
	Previous    *string        `json:"previous"`
	Target      string         `json:"target"`
	Transaction *string        `json:"transaction"`
	Message     string         `json:"message,omitempty"`
	Action      string         `json:"action"`
	Decisions   []GateDecision `json:"decisions,omitempty"`
	// OmittedDecisions counts decisions beyond MaxGateDecisions.
	OmittedDecisions int `json:"omittedDecisions,omitempty"`
}

// GateDecision is one redacted resource decision of a gate outcome.
type GateDecision struct {
	Resource string `json:"resource"`
	Scope    string `json:"scope,omitempty"`
	Decision string `json:"decision"`
	Result   string `json:"result,omitempty"`
}

// Refusal is one independent typed refusal and its next step.
type Refusal struct {
	Source   string         `json:"source"`
	Code     string         `json:"code"`
	Resource string         `json:"resource,omitempty"`
	Text     string         `json:"text"`
	Next     string         `json:"next,omitempty"`
	Action   *RefusalAction `json:"action,omitempty"`
}

// RefusalAction is a typed recovery command and its rendered form.
type RefusalAction struct {
	Verb    string `json:"verb"`
	Yard    string `json:"yard,omitempty"`
	Version string `json:"version,omitempty"`
	Command string `json:"command"`
}

// RefusalError builds an error whose data holds refusals within the bounds.
// It returns the plain error when there is nothing typed to attach.
func RefusalError(code ErrorCode, message string, data ErrorData) *Error {
	fault := &Error{Code: code, Message: message}
	if len(data.Refusals) == 0 {
		return fault
	}
	data = data.bounded()
	data.SchemaVersion = errorDataSchema
	encoded, err := json.Marshal(data)
	if err != nil {
		return fault
	}
	fault.Data = encoded
	return fault
}

// DecodeErrorData reads the refusals of a peer's error within the same bounds
// the owner applies. It reports false for absent, unknown or empty data.
func DecodeErrorData(fault *Error) (ErrorData, bool) {
	var data ErrorData
	if fault == nil || len(fault.Data) == 0 || json.Unmarshal(fault.Data, &data) != nil ||
		data.SchemaVersion != errorDataSchema || len(data.Refusals) == 0 {
		return ErrorData{}, false
	}
	return data.bounded(), true
}

// bounded caps the lists and cleans every text without mutating the caller's
// slices; it is the single place the bounds are applied.
func (data ErrorData) bounded() ErrorData {
	if data.Omitted < 0 {
		data.Omitted = 0
	}
	if len(data.Refusals) > MaxErrorRefusals {
		data.Omitted += len(data.Refusals) - MaxErrorRefusals
		data.Refusals = data.Refusals[:MaxErrorRefusals]
	}
	refusals := make([]Refusal, len(data.Refusals))
	for index, refusal := range data.Refusals {
		refusal.Source = boundText(refusal.Source, MaxRefusalName)
		refusal.Code = boundText(refusal.Code, MaxRefusalName)
		refusal.Resource = boundText(refusal.Resource, MaxRefusalName)
		refusal.Text = boundText(refusal.Text, MaxRefusalText)
		refusal.Next = boundText(refusal.Next, MaxRefusalText)
		if refusal.Action != nil {
			action := *refusal.Action
			action.Verb = boundText(action.Verb, MaxRefusalName)
			action.Yard = boundText(action.Yard, MaxRefusalName)
			action.Version = boundText(action.Version, MaxRefusalName)
			action.Command = boundText(action.Command, MaxRefusalText)
			refusal.Action = &action
		}
		refusals[index] = refusal
	}
	data.Refusals = refusals
	if data.Gate != nil {
		gate := *data.Gate
		gate.Status = boundText(gate.Status, MaxRefusalName)
		gate.Code = boundText(gate.Code, MaxRefusalName)
		gate.Active = boundText(gate.Active, MaxRefusalName)
		gate.Previous = boundOptional(gate.Previous, MaxRefusalName)
		gate.Target = boundText(gate.Target, MaxRefusalName)
		gate.Transaction = boundOptional(gate.Transaction, MaxRefusalName)
		gate.Message = boundText(gate.Message, MaxRefusalText)
		gate.Action = boundText(gate.Action, MaxRefusalText)
		if gate.OmittedDecisions < 0 {
			gate.OmittedDecisions = 0
		}
		if len(gate.Decisions) > MaxGateDecisions {
			gate.OmittedDecisions += len(gate.Decisions) - MaxGateDecisions
			gate.Decisions = gate.Decisions[:MaxGateDecisions]
		}
		decisions := make([]GateDecision, len(gate.Decisions))
		for index, decision := range gate.Decisions {
			decisions[index] = GateDecision{
				Resource: boundText(decision.Resource, MaxRefusalName), Scope: boundText(decision.Scope, MaxRefusalName),
				Decision: boundText(decision.Decision, MaxRefusalName), Result: boundText(decision.Result, MaxRefusalText),
			}
		}
		if len(decisions) == 0 {
			decisions = nil
		}
		gate.Decisions = decisions
		data.Gate = &gate
	}
	return data
}

func boundOptional(text *string, limit int) *string {
	if text == nil {
		return nil
	}
	bounded := boundText(*text, limit)
	return &bounded
}

// boundText replaces control characters and cuts text to limit bytes at a
// UTF-8 boundary, marking the cut with an ellipsis.
func boundText(text string, limit int) string {
	text = strings.Map(func(char rune) rune {
		if unicode.IsControl(char) || char == utf8.RuneError {
			return ' '
		}
		return char
	}, strings.ToValidUTF8(text, " "))
	if len(text) <= limit {
		return text
	}
	cut := limit - len("…")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}
