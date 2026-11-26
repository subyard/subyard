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
	Gate any `json:"gate,omitempty"`
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
	data.SchemaVersion = errorDataSchema
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
	encoded, err := json.Marshal(data)
	if err != nil {
		return fault
	}
	fault.Data = encoded
	return fault
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
