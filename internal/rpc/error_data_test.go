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
