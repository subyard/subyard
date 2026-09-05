package releasetransition

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	protocolv1 "github.com/Subyard/Subyard/internal/releasetransition/protocol/v1"
)

const processProtocolDigestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestProcessRequestV1AdapterRoundTripsEveryFrozenField(t *testing.T) {
	const wire = `{"schemaVersion":1,"mode":"converge","runtimeRoot":"/opt/subyard/releases","configHome":"/home/dev/.config/subyard","yard":"","target":"release-b","direction":"activate-target","artifactDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registryDigest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","inheritedSettingIds":["settings.power-mode"],"sourceIngress":{"schemaVersion":1,"kind":"pre-go-source-v1","sourceRoot":"/home/dev/subyard","dataHome":"/home/dev/.subyard","binDir":"/home/dev/.local/bin","rc":"/home/dev/.bashrc","loginRC":"/home/dev/.profile"},"replacement":{"transaction":"tx-old","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"post-activation-scope-v0.11.1","sourceVersion":"0.11.1"},"execution":{"plan":"plan-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","authorization":"grant-token"}}`

	var request ProcessRequest
	if err := json.Unmarshal([]byte(wire), &request); err != nil {
		t.Fatalf("Unmarshal ProcessRequest: %v", err)
	}
	if request.Yard != "" || request.SourceIngress == nil || request.Replacement == nil || request.Execution == nil {
		t.Fatalf("decoded request = %#v", request)
	}
	if request.SourceIngress.LoginRC != "/home/dev/.profile" ||
		request.Replacement.Reason != JournalReplacementPostActivationScopeV0111 ||
		request.Execution.Authorization != "grant-token" {
		t.Fatalf("decoded nested request fields = %#v %#v %#v", request.SourceIngress, request.Replacement, request.Execution)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal ProcessRequest: %v", err)
	}
	if got := string(payload); got != wire {
		t.Fatalf("round-trip request\n got: %s\nwant: %s", got, wire)
	}
}

func TestProcessResponseV1AdapterRoundTripsEveryFrozenField(t *testing.T) {
	const inspectionWire = `{"schemaVersion":1,"activationReconciliationOwned":true,"inspection":{"plan":"resume-v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","assessment":{"action":"release.transition.v2","effect":"mutation","changed":true,"impacts":["local-metadata","persistent-data","yard-runtime"],"recovery":"reversible","consequences":["apply the exact typed migration and release activation plan"]},"decisions":[{"resource":"settings.power-mode","scope":"yard","decision":"transform","result":"canonical-v2"}],"blockers":[{"code":"precondition-blocked","resource":"settings.power-mode","message":"resource is busy","retry":"run yard update --check"}],"resume":"tx-0123456789abcdef","outcome":{"status":"operator-action-required","reachedGoal":false,"active":"release-a","previous":"release-z","target":"release-b","code":"precondition-blocked","message":"resource is busy","retry":"run yard update --check","transaction":"tx-0123456789abcdef","warnings":["warning a","warning b"]}}}`
	const outcomeWire = `{"schemaVersion":1,"activationReconciliationOwned":false,"outcome":{"status":"ready","reachedGoal":true,"active":"release-b","previous":"release-a","target":"release-b","code":"ready","message":"verified","transaction":"tx-0123456789abcdef","warnings":["warning a","warning b"]}}`

	for name, wire := range map[string]string{"inspection": inspectionWire, "outcome": outcomeWire} {
		t.Run(name, func(t *testing.T) {
			var response ProcessResponse
			if err := json.Unmarshal([]byte(wire), &response); err != nil {
				t.Fatalf("Unmarshal ProcessResponse: %v", err)
			}
			if name == "inspection" {
				if response.Inspection == nil || response.Inspection.Resume == nil ||
					len(response.Inspection.Decisions) != 1 || len(response.Inspection.Blockers) != 1 ||
					response.Inspection.Outcome == nil || len(response.Inspection.Outcome.Warnings) != 2 {
					t.Fatalf("decoded inspection response = %#v", response)
				}
			} else if response.Outcome == nil || response.Outcome.Previous == nil ||
				response.Outcome.Transaction == nil || len(response.Outcome.Warnings) != 2 {
				t.Fatalf("decoded convergence response = %#v", response)
			}
			payload, err := json.Marshal(response)
			if err != nil {
				t.Fatalf("Marshal ProcessResponse: %v", err)
			}
			if got := string(payload); got != wire {
				t.Fatalf("round-trip response\n got: %s\nwant: %s", got, wire)
			}
		})
	}
}

func TestProcessV1AdaptersClonePointersAndSlices(t *testing.T) {
	request := ProcessRequest{
		SchemaVersion: ProcessProtocolSchemaV1, Mode: ProcessConverge,
		RuntimeRoot: "/opt/subyard/releases", ConfigHome: "/home/dev/.config/subyard",
		Target: "release-b", Direction: DirectionActivateTarget,
		ArtifactDigest: processProtocolDigestA, InheritedSettingIDs: []string{"settings.power-mode"},
		SourceIngress: &SourceIngressRequest{
			SchemaVersion: SourceIngressRequestSchemaV1, Kind: SourceIngressPreGoV1,
			SourceRoot: "/home/dev/subyard", DataHome: "/home/dev/.subyard",
			BinDir: "/home/dev/.local/bin", RC: "/home/dev/.bashrc", LoginRC: "/home/dev/.profile",
		},
		Execution: &Execution{Plan: "unrecognized-plan", Authorization: "grant-token"},
	}
	wireRequest := processRequestV1(request)
	wireRequest.InheritedSettingIDs[0] = "changed"
	wireRequest.SourceIngress.SourceRoot = "/changed"
	if request.InheritedSettingIDs[0] != "settings.power-mode" || request.SourceIngress.SourceRoot != "/home/dev/subyard" {
		t.Fatal("processRequestV1 shared mutable request storage")
	}
	internalRequest := processRequestFromV1(wireRequest)
	wireRequest.InheritedSettingIDs[0] = "changed-again"
	wireRequest.SourceIngress.SourceRoot = "/changed-again"
	if internalRequest.InheritedSettingIDs[0] != "changed" || internalRequest.SourceIngress.SourceRoot != "/changed" {
		t.Fatal("processRequestFromV1 shared mutable request storage")
	}

	transaction := TransactionID("tx-0123456789abcdef")
	previous := ReleaseID("release-a")
	response := ProcessResponse{
		SchemaVersion: ProcessProtocolSchemaV1,
		Inspection: &Inspection{
			Plan: PlanToken("resume-v1-" + strings.Repeat("b", 64)), Resume: &transaction,
			Assessment: domain.ActionAssessment{
				Action: "release.transition.v2", Effect: domain.ActionMutation, Changed: true,
				Impacts: []domain.ActionImpact{
					domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
				},
				Recovery: domain.RecoveryReversible, Consequences: []string{"consequence"},
			},
			Decisions: []RedactedDecision{{Resource: "resource", Decision: DecisionPreserve}},
			Blockers:  []Blocker{{Code: CodePlanStale, Message: "blocked", Retry: "run yard update --check"}},
			Outcome: &Outcome{
				Status: StatusOperatorActionRequired, Active: "release-b", Previous: &previous,
				Target: "release-c", Code: CodePlanStale, Message: "blocked",
				Retry: "run yard update --check", Transaction: &transaction, Warnings: []string{"warning"},
			},
		},
	}
	wireResponse := processResponseV1(response)
	wireResponse.Inspection.Assessment.Impacts[0] = "changed"
	wireResponse.Inspection.Assessment.Consequences[0] = "changed"
	wireResponse.Inspection.Decisions[0].Resource = "changed"
	wireResponse.Inspection.Blockers[0].Message = "changed"
	wireResponse.Inspection.Outcome.Warnings[0] = "changed"
	*wireResponse.Inspection.Resume = "changed"
	if response.Inspection.Assessment.Impacts[0] != domain.ImpactLocalMetadata ||
		response.Inspection.Assessment.Consequences[0] != "consequence" ||
		response.Inspection.Decisions[0].Resource != "resource" || response.Inspection.Blockers[0].Message != "blocked" ||
		response.Inspection.Outcome.Warnings[0] != "warning" || *response.Inspection.Resume != transaction {
		t.Fatal("processResponseV1 shared mutable response storage")
	}
	internalInspection := inspectionFromV1(*wireResponse.Inspection)
	wireResponse.Inspection.Assessment.Impacts[0] = "changed-again"
	wireResponse.Inspection.Assessment.Consequences[0] = "changed-again"
	wireResponse.Inspection.Decisions[0].Resource = "changed-again"
	wireResponse.Inspection.Blockers[0].Message = "changed-again"
	wireResponse.Inspection.Outcome.Warnings[0] = "changed-again"
	*wireResponse.Inspection.Resume = "changed-again"
	if internalInspection.Assessment.Impacts[0] != "changed" ||
		internalInspection.Assessment.Consequences[0] != "changed" ||
		internalInspection.Decisions[0].Resource != "changed" || internalInspection.Blockers[0].Message != "changed" ||
		internalInspection.Outcome.Warnings[0] != "changed" || *internalInspection.Resume != "changed" {
		t.Fatal("inspectionFromV1 shared mutable response storage")
	}

	nilSlices := processRequestFromV1(protocolv1.Request{})
	if nilSlices.InheritedSettingIDs != nil {
		t.Fatalf("nil inherited setting IDs became %#v", nilSlices.InheritedSettingIDs)
	}
	nilInspection := inspectionFromV1(protocolv1.Inspection{})
	if nilInspection.Assessment.Impacts != nil || nilInspection.Assessment.Consequences != nil ||
		nilInspection.Decisions != nil || nilInspection.Blockers != nil {
		t.Fatalf("nil inspection slices were not preserved: %#v", nilInspection)
	}
}

func TestProcessMarshalRejectsUnknownInternalEnums(t *testing.T) {
	validRequest := ProcessRequest{
		SchemaVersion: ProcessProtocolSchemaV1, Mode: ProcessInspect,
		RuntimeRoot: "/opt/subyard/releases", ConfigHome: "/home/dev/.config/subyard",
		Target: "release-b", Direction: DirectionActivateTarget, ArtifactDigest: processProtocolDigestA,
	}
	validOutcome := Outcome{
		Status: StatusReady, ReachedGoal: true, Active: "release-b", Target: "release-b",
		Code: CodeReady, Message: "verified",
	}
	for name, value := range map[string]any{
		"request mode": func() ProcessRequest {
			request := validRequest
			request.Mode = "future"
			return request
		}(),
		"request direction": func() ProcessRequest {
			request := validRequest
			request.Direction = "future"
			return request
		}(),
		"response status": ProcessResponse{
			SchemaVersion: ProcessProtocolSchemaV1,
			Outcome: func() *Outcome {
				outcome := validOutcome
				outcome.Status = "future"
				return &outcome
			}(),
		},
		"response code": ProcessResponse{
			SchemaVersion: ProcessProtocolSchemaV1,
			Outcome: func() *Outcome {
				outcome := validOutcome
				outcome.Code = "future"
				return &outcome
			}(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := json.Marshal(value); err == nil {
				t.Fatal("unknown internal enum was serialized")
			}
		})
	}
}
