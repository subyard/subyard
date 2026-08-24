package testimpact

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteJSONUsesVersionedSnakeCaseWireShape(t *testing.T) {
	path := "internal/testimpact/render.go"
	result := Result{
		SchemaVersion: 1,
		Status:        "selected",
		Changes: []Change{{
			Status: "M", OldPath: &path, NewPath: &path,
			OldMode: "100644", NewMode: "100755",
		}},
		CheckSets:   []string{"go:testimpact"},
		RiskDomains: []string{},
		HostFreeChecks: []CheckRecommendation{{
			ID: "go:testimpact", Tier: "T1", BudgetSeconds: 180,
			Rationale: "selector package self-check",
		}},
		E2EChecks: []CheckRecommendation{},
		FullP0: FullP0Selection{Required: true, Reasons: []FullP0Reason{{
			Code: "standalone_full_domain", RiskDomains: []string{"selector"},
		}}},
		Reasons: []SelectionReason{{
			Path: path, Side: "new", RuleID: "selector-self",
			CheckSets: []string{"go:testimpact"}, RiskDomains: []string{},
		}},
		Errors: []ResultError{{Code: "EXAMPLE", Message: "example diagnostic"}},
	}

	var output bytes.Buffer
	if err := WriteJSON(&output, result); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	want := `{"schema_version":1,"status":"selected","changes":[{"status":"M","similarity":null,"old_path":"internal/testimpact/render.go","new_path":"internal/testimpact/render.go","old_mode":"100644","new_mode":"100755"}],"check_sets":["go:testimpact"],"risk_domains":[],"host_free_checks":[{"id":"go:testimpact","tier":"T1","budget_seconds":180,"rationale":"selector package self-check"}],"e2e_checks":[],"full_p0":{"required":true,"reasons":[{"code":"standalone_full_domain","risk_domains":["selector"]}]},"reasons":[{"path":"internal/testimpact/render.go","side":"new","rule_id":"selector-self","check_sets":["go:testimpact"],"risk_domains":[]}],"errors":[{"code":"EXAMPLE","message":"example diagnostic"}]}` + "\n"
	if output.String() != want {
		t.Fatalf("WriteJSON() = %q, want %q", output.String(), want)
	}
	if strings.Contains(output.String(), `"OldPath"`) || strings.Contains(output.String(), `"NewMode"`) {
		t.Fatalf("WriteJSON() exposed Go field names: %s", output.String())
	}
}

func TestWriteJSONUsesArraysForEveryEmptyCollection(t *testing.T) {
	result := Result{SchemaVersion: 1, Status: "selected"}

	var output bytes.Buffer
	if err := WriteJSON(&output, result); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	want := `{"schema_version":1,"status":"selected","changes":[],"check_sets":[],"risk_domains":[],"host_free_checks":[],"e2e_checks":[],"full_p0":{"required":false,"reasons":[]},"reasons":[],"errors":[]}` + "\n"
	if output.String() != want {
		t.Fatalf("WriteJSON() = %q, want %q", output.String(), want)
	}
}

func TestWriteErrorJSONUsesMinimalShape(t *testing.T) {
	var output bytes.Buffer
	if err := WriteErrorJSON(&output, []ResultError{{Code: "CLI_MISUSE", Message: "invalid command line"}}); err != nil {
		t.Fatalf("WriteErrorJSON() error = %v", err)
	}
	want := `{"schema_version":1,"status":"error","errors":[{"code":"CLI_MISUSE","message":"invalid command line"}]}` + "\n"
	if output.String() != want {
		t.Fatalf("WriteErrorJSON() = %q, want %q", output.String(), want)
	}
}

func TestWriteHumanEscapesControlsAndIsDeterministic(t *testing.T) {
	path := "dir/line\nnext\t\x1b[31m"
	result := Result{
		SchemaVersion: 1,
		Status:        "fallback",
		Changes: []Change{{
			Status: "M", OldPath: &path, NewPath: &path,
			OldMode: "100644", NewMode: "100644",
		}},
		CheckSets:      []string{"host-free:all"},
		RiskDomains:    []string{},
		HostFreeChecks: []CheckRecommendation{},
		E2EChecks:      []CheckRecommendation{},
		FullP0: FullP0Selection{Required: true, Reasons: []FullP0Reason{{
			Code: "universal_fallback", RiskDomains: []string{},
		}}},
		Reasons: []SelectionReason{},
		Errors:  []ResultError{{Code: "UNMATCHED_PATH", Message: "analysis failed\n\x1b[2J"}},
	}

	var first, second bytes.Buffer
	if err := WriteHuman(&first, result); err != nil {
		t.Fatalf("WriteHuman(first) error = %v", err)
	}
	if err := WriteHuman(&second, result); err != nil {
		t.Fatalf("WriteHuman(second) error = %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("WriteHuman() differs across calls:\n%q\n%q", first.String(), second.String())
	}
	if bytes.Contains(first.Bytes(), []byte{0x1b}) || strings.Contains(first.String(), "line\nnext") {
		t.Fatalf("WriteHuman() emitted raw terminal controls: %q", first.String())
	}
	for _, want := range []string{`status: "fallback"`, `"dir/line\nnext\t\x1b[31m"`, `"analysis failed\n\x1b[2J"`} {
		if !strings.Contains(first.String(), want) {
			t.Errorf("WriteHuman() = %q, missing %q", first.String(), want)
		}
	}
}

func TestRenderersRejectInvalidUTF8WithoutReplacement(t *testing.T) {
	invalid := string([]byte{'b', 'a', 'd', 0xff})
	result := Result{
		SchemaVersion:  1,
		Status:         "selected",
		Changes:        []Change{},
		CheckSets:      []string{},
		RiskDomains:    []string{},
		HostFreeChecks: []CheckRecommendation{},
		E2EChecks:      []CheckRecommendation{},
		FullP0:         FullP0Selection{Reasons: []FullP0Reason{}},
		Reasons:        []SelectionReason{},
		Errors:         []ResultError{{Code: "INVALID", Message: invalid}},
	}

	for name, render := range map[string]func(*bytes.Buffer, Result) error{
		"JSON":  func(output *bytes.Buffer, value Result) error { return WriteJSON(output, value) },
		"human": func(output *bytes.Buffer, value Result) error { return WriteHuman(output, value) },
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := render(&output, result); err == nil {
				t.Fatal("renderer accepted invalid UTF-8")
			}
			if output.Len() != 0 {
				t.Fatalf("renderer wrote partial output %q", output.String())
			}
		})
	}
}
