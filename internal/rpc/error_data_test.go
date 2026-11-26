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
	refusals[0] = Refusal{Source: RefusalSourceGate, Code: strings.Repeat("c", 300),
		Text: strings.Repeat("é", MaxRefusalText), Next: "line\nbreak\x1b[31m",
		Action: &RefusalAction{Verb: "start", Command: strings.Repeat("y", 600)}}
	fault := RefusalError(CodePlanFailed, "message", ErrorData{Refusals: refusals, Omitted: 1})
	var data ErrorData
	if err := json.Unmarshal(fault.Data, &data); err != nil {
		t.Fatal(err)
	}
	first := data.Refusals[0]
	if data.SchemaVersion != 1 || len(data.Refusals) != MaxErrorRefusals || data.Omitted != 4 ||
		len(first.Code) > MaxRefusalName || len(first.Text) > MaxRefusalText || !utf8.ValidString(first.Text) ||
		!strings.HasSuffix(first.Text, "…") || first.Next != "line break [31m" ||
		len(first.Action.Command) > MaxRefusalText || fault.Message != "message" {
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

func TestRefusalErrorBoundsTheGateAndDecodeAppliesTheSameBounds(t *testing.T) {
	previous := "release\x1b[0m-a"
	gate := &GateOutcome{Status: "migration-required", Code: "transition-required",
		Active: strings.Repeat("a", 300), Previous: &previous, Target: "b",
		Message: strings.Repeat("m\n", 600), Action: "run yard migrate\r"}
	for index := 0; index < MaxGateDecisions+2; index++ {
		gate.Decisions = append(gate.Decisions, GateDecision{Resource: "r", Decision: "apply", Result: strings.Repeat("x", 900)})
	}
	fault := RefusalError(CodeInternal, "m", ErrorData{Refusals: []Refusal{{Source: RefusalSourceGate, Code: "c", Text: "t"}}, Gate: gate})
	data, ok := DecodeErrorData(fault)
	if !ok || data.Gate == nil {
		t.Fatalf("data = %s", fault.Data)
	}
	bounded := data.Gate
	if len(bounded.Message) > MaxRefusalText || strings.Contains(bounded.Message, "\n") ||
		len(bounded.Active) > MaxRefusalName || *bounded.Previous != "release [0m-a" || bounded.Action != "run yard migrate " ||
		len(bounded.Decisions) != MaxGateDecisions || bounded.OmittedDecisions != 2 ||
		len(bounded.Decisions[0].Result) > MaxRefusalText || data.Incomplete || data.Omitted != 0 {
		t.Fatalf("gate = %+v", bounded)
	}
	if len(gate.Decisions) != MaxGateDecisions+2 || gate.Message != strings.Repeat("m\n", 600) {
		t.Fatal("caller gate was mutated")
	}
	// A peer's unbounded data is bounded the same way on decode.
	raw, _ := json.Marshal(ErrorData{SchemaVersion: 1, Refusals: []Refusal{{Source: "config", Code: "c", Text: "t\x07" + strings.Repeat("é", 400)}}, Gate: gate})
	data, ok = DecodeErrorData(&Error{Code: CodeInternal, Data: raw})
	if !ok || len(data.Refusals[0].Text) > MaxRefusalText || strings.ContainsRune(data.Refusals[0].Text, 7) ||
		len(data.Gate.Message) > MaxRefusalText || len(data.Gate.Decisions) != MaxGateDecisions {
		t.Fatalf("decoded = %+v", data)
	}
	for _, invalid := range []string{``, `{}`, `{"schemaVersion":2,"refusals":[{"source":"config","code":"c","text":"t"}]}`, `{"schemaVersion":1,"refusals":[]}`, `[]`} {
		if _, ok := DecodeErrorData(&Error{Data: json.RawMessage(invalid)}); ok {
			t.Fatalf("accepted %q", invalid)
		}
	}
}
