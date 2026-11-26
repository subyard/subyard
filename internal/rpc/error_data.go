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
	Gate *GateOutcome `json:"gate,omitempty"`
}

// GateOutcome is the public release-gate outcome of a gate refusal; its
// blockers are the refusals.
type GateOutcome struct {
	Status      string  `json:"status"`
	Code        string  `json:"code"`
	Active      string  `json:"active"`
	Target      string  `json:"target"`
	Transaction *string `json:"transaction"`
	Message     string  `json:"message,omitempty"`
	Action      string  `json:"action"`
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
	Command string `json:"command"`
}

// RefusalError builds an error whose data holds refusals within the bounds.
// It returns the plain error when there is nothing typed to attach.
func RefusalError(code ErrorCode, message string, data ErrorData) *Error {
	fault := &Error{Code: code, Message: message}
	data = data.bounded()
	if len(data.Refusals) == 0 {
		return fault
	}
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
		data.SchemaVersion != errorDataSchema {
		return ErrorData{}, false
	}
	data = data.bounded()
	return data, len(data.Refusals) != 0
}

// bounded caps the list, cleans every text and keeps only refusals every client
// accepts, without mutating the caller's slices. It is the single place the
// bounds and the client validation rules are applied: a refusal with an unknown
// source, an invalid code or an empty text is dropped and marks the data
// incomplete; an action whose verb or command is invalid becomes prose in Next.
func (data ErrorData) bounded() ErrorData {
	if data.Omitted < 0 {
		data.Omitted = 0
	}
	refusals := []Refusal{}
	for _, refusal := range data.Refusals {
		if (refusal.Source != RefusalSourceConfig && refusal.Source != RefusalSourceGate) ||
			!ValidRefusalName(refusal.Code) || refusal.Text == "" {
			data.Incomplete = true
			continue
		}
		if len(refusals) == MaxErrorRefusals {
			data.Omitted++
			continue
		}
		refusal.Resource = BoundText(refusal.Resource, MaxRefusalName)
		refusal.Text = BoundText(refusal.Text, MaxRefusalText)
		if refusal.Action != nil {
			action := RefusalAction{Verb: refusal.Action.Verb, Command: BoundText(refusal.Action.Command, MaxRefusalText)}
			if ValidRefusalName(action.Verb) && strings.HasPrefix(action.Command, "yard ") {
				refusal.Action = &action
			} else {
				refusal.Action = nil
				if strings.TrimSpace(action.Command) != "" {
					refusal.Next = strings.TrimPrefix(refusal.Next+", then run "+action.Command, ", then ")
				}
			}
		}
		refusal.Next = BoundText(refusal.Next, MaxRefusalText)
		refusals = append(refusals, refusal)
	}
	data.Refusals = refusals
	if data.Gate != nil {
		gate := *data.Gate
		gate.Status = BoundText(gate.Status, MaxRefusalName)
		gate.Code = BoundText(gate.Code, MaxRefusalName)
		gate.Active = BoundText(gate.Active, MaxRefusalName)
		gate.Target = BoundText(gate.Target, MaxRefusalName)
		if gate.Transaction != nil {
			transaction := BoundText(*gate.Transaction, MaxRefusalName)
			gate.Transaction = &transaction
		}
		gate.Message = BoundText(gate.Message, MaxRefusalText)
		gate.Action = BoundText(gate.Action, MaxRefusalText)
		data.Gate = &gate
	}
	return data
}

// ValidRefusalName reports a refusal code or action verb every client accepts:
// 1 to MaxRefusalName bytes of lowercase ASCII letters, digits, '-' and '_'.
func ValidRefusalName(name string) bool {
	if name == "" || len(name) > MaxRefusalName {
		return false
	}
	for _, char := range []byte(name) {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

// BoundText replaces control and format characters (such as bidirectional
// overrides) and cuts text to limit bytes at a UTF-8 boundary, marking the cut
// with an ellipsis. Peer text passes through it before it reaches a terminal.
func BoundText(text string, limit int) string {
	text = strings.Map(func(char rune) rune {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) || char == utf8.RuneError {
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
