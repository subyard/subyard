package rpc

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRefusalErrorBoundsAndSanitizesData(t *testing.T) {
	refusals := make([]Refusal, MaxErrorRefusals+3)
	for index := range refusals {
		refusals[index] = Refusal{Source: RefusalSourceConfig, Code: "c", Text: "t"}
	}
	refusals[0] = Refusal{Source: RefusalSourceGate, Code: strings.Repeat("c", MaxRefusalName),
		Resource: strings.Repeat("r", 300), Text: strings.Repeat("é", MaxRefusalText), Next: "line\nbreak\x1b[31m",
		Action: &RefusalAction{Verb: "start", Command: "yard " + strings.Repeat("y", 600)}}
	fault := RefusalError(CodePlanFailed, "message", ErrorData{Refusals: refusals, Omitted: 1})
	var data ErrorData
	if err := json.Unmarshal(fault.Data, &data); err != nil {
		t.Fatal(err)
	}
	first := data.Refusals[0]
	if data.SchemaVersion != 1 || len(data.Refusals) != MaxErrorRefusals || data.Omitted != 4 || data.Incomplete ||
		len(first.Resource) > MaxRefusalName || len(first.Text) > MaxRefusalText || !utf8.ValidString(first.Text) ||
		!strings.HasSuffix(first.Text, "…") || first.Next != "line break [31m" ||
		len(first.Action.Command) > MaxRefusalText || !strings.HasPrefix(first.Action.Command, "yard ") || fault.Message != "message" {
		t.Fatalf("data = %+v", data)
	}
	if refusals[0].Next != "line\nbreak\x1b[31m" {
		t.Fatal("caller refusals were mutated")
	}
}

func TestRefusalErrorWithoutRefusalsHasNoData(t *testing.T) {
	if fault := RefusalError(CodeInternal, "m", ErrorData{Incomplete: true}); fault.Data != nil {
		t.Fatalf("data = %s", fault.Data)
	}
}

// The owner applies the validation Veranda applies, so Veranda never drops
// data the owner sent: each invalid refusal is left out and marks the data
// incomplete, and an invalid action becomes prose.
func TestRefusalErrorKeepsOnlyRefusalsEveryClientAccepts(t *testing.T) {
	valid := Refusal{Source: RefusalSourceConfig, Code: "setting-numeric", Text: "t"}
	for name, refusal := range map[string]Refusal{
		"unknown source": {Source: "shell", Code: "c", Text: "t"},
		"uppercase code": {Source: RefusalSourceConfig, Code: "Setting", Text: "t"},
		"path code":      {Source: RefusalSourceConfig, Code: "../x", Text: "t"},
		"space code":     {Source: RefusalSourceConfig, Code: "a b", Text: "t"},
		"empty code":     {Source: RefusalSourceConfig, Text: "t"},
		"long code":      {Source: RefusalSourceConfig, Code: strings.Repeat("c", MaxRefusalName+1), Text: "t"},
		"empty text":     {Source: RefusalSourceConfig, Code: "c"},
	} {
		t.Run(name, func(t *testing.T) {
			data, ok := DecodeErrorData(RefusalError(CodeInternal, "m", ErrorData{Refusals: []Refusal{refusal, valid}}))
			if !ok || len(data.Refusals) != 1 || data.Refusals[0] != valid || !data.Incomplete {
				t.Fatalf("data = %+v", data)
			}
			if fault := RefusalError(CodeInternal, "m", ErrorData{Refusals: []Refusal{refusal}}); fault.Data != nil {
				t.Fatalf("only an invalid refusal still sent data %s", fault.Data)
			}
		})
	}
	for name, test := range map[string]struct {
		action RefusalAction
		next   string
	}{
		"uppercase verb": {RefusalAction{Verb: "Start", Command: "yard start"}, "restore it, then run yard start"},
		"empty verb":     {RefusalAction{Command: "yard start"}, "restore it, then run yard start"},
		"other program":  {RefusalAction{Verb: "start", Command: "rm -rf /"}, "restore it, then run rm -rf /"},
		"bare yard":      {RefusalAction{Verb: "start", Command: "yard"}, "restore it, then run yard"},
		"empty command":  {RefusalAction{Verb: "start"}, "restore it"},
	} {
		t.Run(name, func(t *testing.T) {
			action := test.action
			refusal := Refusal{Source: RefusalSourceGate, Code: "c", Text: "t", Next: "restore it", Action: &action}
			data, ok := DecodeErrorData(RefusalError(CodeInternal, "m", ErrorData{Refusals: []Refusal{refusal}}))
			if !ok || data.Incomplete || data.Refusals[0].Action != nil || data.Refusals[0].Next != test.next {
				t.Fatalf("data = %+v", data)
			}
		})
	}
	action := RefusalAction{Verb: "start", Command: "yard -Y default start"}
	data, _ := DecodeErrorData(RefusalError(CodeInternal, "m", ErrorData{Refusals: []Refusal{{Source: RefusalSourceGate, Code: "c", Text: "t", Action: &action}}}))
	if data.Refusals[0].Action == nil || *data.Refusals[0].Action != action || data.Refusals[0].Next != "" {
		t.Fatalf("valid action changed: %+v", data.Refusals[0])
	}
}

func TestRefusalErrorBoundsTheGateAndDecodeAppliesTheSameBounds(t *testing.T) {
	transaction := "tx\x1b[0m-a"
	gate := &GateOutcome{Status: "migration-required", Code: "transition-required",
		Active: strings.Repeat("a", 300), Transaction: &transaction, Target: "b",
		Message: strings.Repeat("m\n", 600), Action: "run yard migrate\r"}
	fault := RefusalError(CodeInternal, "m", ErrorData{Refusals: []Refusal{{Source: RefusalSourceGate, Code: "c", Text: "t"}}, Gate: gate})
	data, ok := DecodeErrorData(fault)
	if !ok || data.Gate == nil {
		t.Fatalf("data = %s", fault.Data)
	}
	bounded := data.Gate
	if len(bounded.Message) > MaxRefusalText || strings.Contains(bounded.Message, "\n") ||
		len(bounded.Active) > MaxRefusalName || *bounded.Transaction != "tx [0m-a" || bounded.Action != "run yard migrate " ||
		data.Incomplete || data.Omitted != 0 {
		t.Fatalf("gate = %+v", bounded)
	}
	if gate.Message != strings.Repeat("m\n", 600) || transaction != "tx\x1b[0m-a" {
		t.Fatal("caller gate was mutated")
	}
	// A peer's unbounded data is bounded the same way on decode.
	raw, _ := json.Marshal(ErrorData{SchemaVersion: 1, Refusals: []Refusal{{Source: "config", Code: "c", Text: "t\x07" + strings.Repeat("é", 400)}}, Gate: gate})
	data, ok = DecodeErrorData(&Error{Code: CodeInternal, Data: raw})
	if !ok || len(data.Refusals[0].Text) > MaxRefusalText || strings.ContainsRune(data.Refusals[0].Text, 7) ||
		len(data.Gate.Message) > MaxRefusalText {
		t.Fatalf("decoded = %+v", data)
	}
	for _, invalid := range []string{``, `{}`, `{"schemaVersion":2,"refusals":[{"source":"config","code":"c","text":"t"}]}`, `{"schemaVersion":1,"refusals":[]}`, `[]`,
		`{"schemaVersion":1,"refusals":[{"source":"config","code":"C","text":"t"}]}`} {
		if _, ok := DecodeErrorData(&Error{Data: json.RawMessage(invalid)}); ok {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestBoundTextReplacesControlAndFormatCharacters(t *testing.T) {
	// U+202E and U+2066 reorder terminal text; U+200B and U+FEFF hide in it.
	got := BoundText("safe\u202Eexe.txt\u2066x\u2069\u200b\ufeff\x00\x1b[0m", MaxRefusalText)
	if got != "safe exe.txt x     [0m" {
		t.Fatalf("got %q", got)
	}
	if got := BoundText("café ok", MaxRefusalText); got != "café ok" {
		t.Fatalf("ordinary text changed: %q", got)
	}
}
