package v1

import (
	"bytes"
	"strings"
	"testing"
)

const (
	digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	planA   = "plan-v1-" + digestA
	resumeB = "resume-v1-" + digestB
)

const inspectResponseJSON = `{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"decisions":[{"resource":"settings.power-mode","scope":"yard","decision":"transform","result":"canonical-v2"}],"outcome":{"status":"migration-required","reachedGoal":false,"active":"release-a","target":"release-b","code":"transition-required","message":"the release transition has not started","retry":"run yard update"}}}`

const convergeResponseJSON = `{"schemaVersion":1,"activationReconciliationOwned":true,"outcome":{"status":"ready","reachedGoal":true,"active":"release-b","previous":"release-a","target":"release-b","code":"ready","message":"verified","transaction":"tx-0123456789abcdef"}}`

const resumeResponseJSON = `{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"resume-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"resume":"tx-0123456789abcdef","outcome":{"status":"recovering","reachedGoal":false,"active":"release-a","target":"release-b","code":"recovery-pending","message":"the transition can resume","retry":"run yard update","transaction":"tx-0123456789abcdef"}}}`

const blockedResponseJSON = `{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"blockers":[{"code":"precondition-blocked","resource":"settings.power-mode","message":"resource is busy","retry":"run yard update --check"}],"outcome":{"status":"operator-action-required","reachedGoal":false,"active":"release-a","target":"release-b","code":"precondition-blocked","message":"resource is busy","retry":"run yard update --check"}}}`

func TestRequestCodecPreservesFrozenV1Wire(t *testing.T) {
	const wire = `{"schemaVersion":1,"mode":"converge","runtimeRoot":"/opt/subyard/releases","configHome":"/home/dev/.config/subyard","yard":"example","target":"release-b","direction":"activate-target","artifactDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registryDigest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","inheritedSettingIds":["settings.power-mode"],"sourceIngress":{"schemaVersion":1,"kind":"pre-go-source-v1","sourceRoot":"/home/dev/subyard","dataHome":"/home/dev/.subyard","binDir":"/home/dev/.local/bin","rc":"/home/dev/.bashrc","loginRC":"/home/dev/.profile"},"replacement":{"transaction":"tx-old","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"post-activation-scope-v0.11.1","sourceVersion":"0.11.1"},"execution":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","authorization":"grant-token"}}`

	request, err := DecodeRequest(strings.NewReader(wire))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if request.SchemaVersion != 1 || request.Mode != "converge" || request.Target != "release-b" {
		t.Fatalf("decoded request = %#v", request)
	}
	if request.SourceIngress == nil || request.SourceIngress.Kind != "pre-go-source-v1" {
		t.Fatalf("decoded source ingress = %#v", request.SourceIngress)
	}
	if request.Replacement == nil || request.Replacement.SourceVersion != "0.11.1" {
		t.Fatalf("decoded replacement = %#v", request.Replacement)
	}
	if request.Execution == nil || request.Execution.Authorization != "grant-token" {
		t.Fatalf("decoded execution = %#v", request.Execution)
	}

	var encoded bytes.Buffer
	if err := EncodeRequest(&encoded, request); err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if got, want := encoded.String(), wire+"\n"; got != want {
		t.Fatalf("encoded request\n got: %s\nwant: %s", got, want)
	}
}

func TestResponseCodecPreservesFrozenV1WireAndConvergence(t *testing.T) {
	inspectionResponse, err := DecodeResponse(strings.NewReader(inspectResponseJSON))
	if err != nil {
		t.Fatalf("DecodeResponse(inspection): %v", err)
	}
	if inspectionResponse.Inspection == nil {
		t.Fatal("decoded response has no inspection")
	}
	goal := Goal{Target: "release-b", Direction: "activate-target"}
	if err := inspectionResponse.Inspection.ValidateOutcome(goal); err != nil {
		t.Fatalf("ValidateOutcome: %v", err)
	}

	convergedResponse, err := DecodeResponse(strings.NewReader(convergeResponseJSON))
	if err != nil {
		t.Fatalf("DecodeResponse(convergence): %v", err)
	}
	if convergedResponse.Outcome == nil {
		t.Fatal("decoded response has no outcome")
	}
	if err := convergedResponse.Outcome.ValidateConvergence(goal, *inspectionResponse.Inspection); err != nil {
		t.Fatalf("ValidateConvergence: %v", err)
	}

	var encoded bytes.Buffer
	if err := EncodeResponse(&encoded, inspectionResponse); err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if got, want := encoded.String(), inspectResponseJSON+"\n"; got != want {
		t.Fatalf("encoded response\n got: %s\nwant: %s", got, want)
	}
}

func TestInspectionAcceptsFrozenResumeAndBlockedStates(t *testing.T) {
	goal := Goal{Target: "release-b", Direction: "activate-target"}
	for name, wire := range map[string]string{
		"resume":  resumeResponseJSON,
		"blocked": blockedResponseJSON,
	} {
		t.Run(name, func(t *testing.T) {
			response, err := DecodeResponse(strings.NewReader(wire))
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			if response.Inspection == nil {
				t.Fatal("decoded response has no inspection")
			}
			if err := response.Inspection.ValidateOutcome(goal); err != nil {
				t.Fatalf("ValidateOutcome: %v", err)
			}
		})
	}
}

func TestCodecsRejectMalformedUnknownTrailingAndOversizedJSON(t *testing.T) {
	tests := []struct {
		name    string
		request bool
		payload string
	}{
		{name: "malformed request", request: true, payload: `{"schemaVersion":1`},
		{name: "unknown request schema", request: true, payload: `{"schemaVersion":2,"mode":"inspect"}`},
		{name: "unknown request field", request: true, payload: `{"schemaVersion":1,"mode":"inspect","newField":true}`},
		{name: "unknown nested request field", request: true, payload: `{"schemaVersion":1,"mode":"inspect","execution":{"plan":"p","authorization":"a","newField":true}}`},
		{name: "unknown response schema", payload: `{"schemaVersion":2,"activationReconciliationOwned":false}`},
		{name: "unknown response field", payload: `{"schemaVersion":1,"activationReconciliationOwned":false,"newField":true}`},
		{name: "unknown nested response field", payload: `{"schemaVersion":1,"activationReconciliationOwned":false,"outcome":{"status":"ready","reachedGoal":true,"active":"release-b","target":"release-b","code":"ready","message":"verified","newField":true}}`},
		{name: "unknown response status", payload: `{"schemaVersion":1,"activationReconciliationOwned":false,"outcome":{"status":"future","reachedGoal":false,"active":"release-a","target":"release-b","code":"plan-stale","message":"cannot continue","retry":"run yard update --check"}}`},
		{name: "trailing request", request: true, payload: `{"schemaVersion":1,"mode":"inspect"} {}`},
		{name: "trailing response", payload: `{"schemaVersion":1,"activationReconciliationOwned":false} false`},
		{name: "oversized request", request: true, payload: strings.Repeat(" ", (1<<20)+1)},
		{name: "oversized response", payload: strings.Repeat(" ", (1<<20)+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.request {
				_, err = DecodeRequest(strings.NewReader(test.payload))
			} else {
				_, err = DecodeResponse(strings.NewReader(test.payload))
			}
			if err == nil {
				t.Fatal("invalid payload was accepted")
			}
			if strings.HasPrefix(test.name, "oversized") && !strings.Contains(err.Error(), "1 MiB") {
				t.Fatalf("oversized payload failed for another reason: %v", err)
			}
		})
	}
}

func TestRequestCodecRejectsInvalidFrozenNestedDescriptors(t *testing.T) {
	for name, nested := range map[string]string{
		"source kind":         `"sourceIngress":{"schemaVersion":1,"kind":"future","sourceRoot":"/home/dev/subyard","dataHome":"/home/dev/.subyard","binDir":"/home/dev/.local/bin","rc":"/home/dev/.bashrc","loginRC":"/home/dev/.profile"}`,
		"source path":         `"sourceIngress":{"schemaVersion":1,"kind":"pre-go-source-v1","sourceRoot":"/","dataHome":"/home/dev/.subyard","binDir":"/home/dev/.local/bin","rc":"/home/dev/.bashrc","loginRC":"/home/dev/.profile"}`,
		"replacement reason":  `"replacement":{"transaction":"tx-old","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"future"}`,
		"replacement version": `"replacement":{"transaction":"tx-old","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"post-activation-scope-v0.11.1","sourceVersion":"01.11.1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			wire := `{"schemaVersion":1,"mode":"inspect",` + nested + `}`
			if _, err := DecodeRequest(strings.NewReader(wire)); err == nil {
				t.Fatal("invalid nested descriptor was accepted")
			}
		})
	}
}

func TestSemanticValidationRejectsUnknownEnumsAndPolicyDisagreement(t *testing.T) {
	response, err := DecodeResponse(strings.NewReader(inspectResponseJSON))
	if err != nil {
		t.Fatal(err)
	}
	base := *response.Inspection
	goal := Goal{Target: "release-b", Direction: "activate-target"}

	tests := []struct {
		name   string
		mutate func(*Inspection)
	}{
		{name: "unknown direction", mutate: func(*Inspection) {}},
		{name: "unknown status", mutate: func(value *Inspection) { value.Outcome.Status = "future" }},
		{name: "unknown code", mutate: func(value *Inspection) { value.Outcome.Code = "future" }},
		{name: "unknown decision", mutate: func(value *Inspection) { value.Decisions[0].Decision = "future" }},
		{name: "assessment effect disagreement", mutate: func(value *Inspection) { value.Assessment.Effect = "destruction" }},
		{name: "assessment recovery disagreement", mutate: func(value *Inspection) { value.Assessment.Recovery = "irreversible" }},
		{name: "assessment impact disagreement", mutate: func(value *Inspection) { value.Assessment.Impacts = []string{"local-metadata"} }},
		{name: "assessment consequence disagreement", mutate: func(value *Inspection) { value.Assessment.Consequences = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := cloneInspection(base)
			caseGoal := goal
			if test.name == "unknown direction" {
				caseGoal.Direction = "sideways"
			} else {
				test.mutate(&inspection)
			}
			if err := inspection.ValidateOutcome(caseGoal); err == nil {
				t.Fatal("semantically invalid inspection was accepted")
			}
		})
	}
}

func TestSemanticValidationPreservesFrozenLimits(t *testing.T) {
	response, err := DecodeResponse(strings.NewReader(inspectResponseJSON))
	if err != nil {
		t.Fatal(err)
	}
	base := *response.Inspection
	goal := Goal{Target: "release-b", Direction: "activate-target"}

	tooManyDecisions := cloneInspection(base)
	tooManyDecisions.Decisions = make([]RedactedDecision, 257)
	for index := range tooManyDecisions.Decisions {
		tooManyDecisions.Decisions[index] = RedactedDecision{Resource: "resource", Decision: "preserve"}
	}
	if err := tooManyDecisions.ValidateOutcome(goal); err == nil {
		t.Fatal("inspection accepted more than 256 decisions")
	}

	tooManyConsequences := cloneInspection(base)
	tooManyConsequences.Assessment.Consequences = make([]string, 65)
	for index := range tooManyConsequences.Assessment.Consequences {
		tooManyConsequences.Assessment.Consequences[index] = "safe consequence"
	}
	if err := tooManyConsequences.ValidateOutcome(goal); err == nil {
		t.Fatal("inspection accepted more than 64 assessment consequences")
	}

	tooManyWarnings := *base.Outcome
	tooManyWarnings.Warnings = make([]string, 65)
	for index := range tooManyWarnings.Warnings {
		tooManyWarnings.Warnings[index] = "safe warning"
	}
	if err := tooManyWarnings.ValidateInspection(goal); err == nil {
		t.Fatal("outcome accepted more than 64 warnings")
	}
}

func TestResponseCodecDefersUnknownLinkConvergenceToGoalValidation(t *testing.T) {
	const wire = `{"schemaVersion":1,"activationReconciliationOwned":true,"outcome":{"status":"operator-action-required","reachedGoal":false,"active":"","target":"release-b","code":"recovery-ambiguous","message":"release facts cannot be observed","retry":"run yard update --check","transaction":"tx-0123456789abcdef"}}`
	response, err := DecodeResponse(strings.NewReader(wire))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	inspectionResponse, err := DecodeResponse(strings.NewReader(inspectResponseJSON))
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{Target: "release-b", Direction: "activate-target"}
	if err := response.Outcome.ValidateConvergence(goal, *inspectionResponse.Inspection); err != nil {
		t.Fatalf("ValidateConvergence: %v", err)
	}
}

func TestEncodeRejectsUnknownSchemaAndOversizedJSON(t *testing.T) {
	for name, encode := range map[string]func() error{
		"request schema":  func() error { return EncodeRequest(&bytes.Buffer{}, Request{SchemaVersion: 2, Mode: "inspect"}) },
		"response schema": func() error { return EncodeResponse(&bytes.Buffer{}, Response{SchemaVersion: 2}) },
		"oversized request": func() error {
			return EncodeRequest(&bytes.Buffer{}, Request{
				SchemaVersion: 1, Mode: "converge", RuntimeRoot: "/opt/subyard/releases",
				ConfigHome: "/home/dev/.config/subyard", Yard: "example", Target: "release-b",
				Direction: "activate-target", ArtifactDigest: digestA,
				Execution: &Execution{Plan: planA, Authorization: strings.Repeat("x", 1<<20)},
			})
		},
		"oversized JSON": func() error {
			return encode(&bytes.Buffer{}, map[string]string{"value": strings.Repeat("x", 1<<20)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := encode(); err == nil {
				t.Fatal("invalid value was encoded")
			}
		})
	}
}

func cloneInspection(value Inspection) Inspection {
	value.Assessment.Impacts = append([]string(nil), value.Assessment.Impacts...)
	value.Assessment.Consequences = append([]string(nil), value.Assessment.Consequences...)
	value.Decisions = append([]RedactedDecision(nil), value.Decisions...)
	value.Blockers = append([]Blocker(nil), value.Blockers...)
	if value.Resume != nil {
		resume := *value.Resume
		value.Resume = &resume
	}
	if value.Outcome != nil {
		outcome := *value.Outcome
		outcome.Warnings = append([]string(nil), value.Outcome.Warnings...)
		value.Outcome = &outcome
	}
	return value
}
