package releasetransition

import (
	"bytes"
	"slices"

	"github.com/Subyard/Subyard/internal/domain"
	protocolv1 "github.com/Subyard/Subyard/internal/releasetransition/protocol/v1"
)

// The process boundary deliberately projects internal models onto frozen wire
// types. Adding a field to the migration engine must not add it to a response
// consumed by an already released updater.
func (request ProcessRequest) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	if err := protocolv1.EncodeRequest(&buffer, processRequestV1(request)); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func (request *ProcessRequest) UnmarshalJSON(payload []byte) error {
	wire, err := protocolv1.DecodeRequest(bytes.NewReader(payload))
	if err != nil {
		return err
	}
	*request = processRequestFromV1(wire)
	return nil
}

func (response ProcessResponse) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	if err := protocolv1.EncodeResponse(&buffer, processResponseV1(response)); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func (response *ProcessResponse) UnmarshalJSON(payload []byte) error {
	wire, err := protocolv1.DecodeResponse(bytes.NewReader(payload))
	if err != nil {
		return err
	}
	*response = ProcessResponse{
		SchemaVersion:                 wire.SchemaVersion,
		ActivationReconciliationOwned: wire.ActivationReconciliationOwned,
		Inspection:                    protocolPointer(wire.Inspection, inspectionFromV1),
		Outcome:                       protocolPointer(wire.Outcome, outcomeFromV1),
	}
	return nil
}

// The released wire semantics determine acceptance at this boundary. Internal
// validation may evolve without changing what another supported release means.
func ValidateProcessInspection(goal Goal, inspection Inspection) error {
	return inspectionV1(inspection).ValidateOutcome(goalV1(goal))
}

func ValidateProcessOutcome(goal Goal, outcome Outcome) error {
	return outcomeV1(outcome).ValidateInspection(goalV1(goal))
}

func ValidateProcessConvergence(goal Goal, inspection Inspection, outcome Outcome) error {
	return outcomeV1(outcome).ValidateConvergence(goalV1(goal), inspectionV1(inspection))
}

func processRequestV1(request ProcessRequest) protocolv1.Request {
	return protocolv1.Request{
		SchemaVersion: request.SchemaVersion, Mode: string(request.Mode),
		RuntimeRoot: request.RuntimeRoot, ConfigHome: request.ConfigHome, Yard: request.Yard,
		Target: string(request.Target), Direction: string(request.Direction),
		ArtifactDigest: string(request.ArtifactDigest), RegistryDigest: string(request.RegistryDigest),
		InheritedSettingIDs: slices.Clone(request.InheritedSettingIDs),
		SourceIngress:       protocolPointer(request.SourceIngress, sourceIngressV1),
		Replacement:         protocolPointer(request.Replacement, replacementV1),
		Execution: protocolPointer(request.Execution, func(value Execution) protocolv1.Execution {
			return protocolv1.Execution{Plan: string(value.Plan), Authorization: string(value.Authorization)}
		}),
	}
}

func processRequestFromV1(request protocolv1.Request) ProcessRequest {
	return ProcessRequest{
		SchemaVersion: request.SchemaVersion, Mode: ProcessMode(request.Mode),
		RuntimeRoot: request.RuntimeRoot, ConfigHome: request.ConfigHome, Yard: request.Yard,
		Target: ReleaseID(request.Target), Direction: Direction(request.Direction),
		ArtifactDigest: Fingerprint(request.ArtifactDigest), RegistryDigest: Fingerprint(request.RegistryDigest),
		InheritedSettingIDs: slices.Clone(request.InheritedSettingIDs),
		SourceIngress:       protocolPointer(request.SourceIngress, sourceIngressFromV1),
		Replacement:         protocolPointer(request.Replacement, replacementFromV1),
		Execution: protocolPointer(request.Execution, func(value protocolv1.Execution) Execution {
			return Execution{Plan: PlanToken(value.Plan), Authorization: Authorization(value.Authorization)}
		}),
	}
}

func processResponseV1(response ProcessResponse) protocolv1.Response {
	return protocolv1.Response{
		SchemaVersion:                 response.SchemaVersion,
		ActivationReconciliationOwned: response.ActivationReconciliationOwned,
		Inspection:                    protocolPointer(response.Inspection, inspectionV1),
		Outcome:                       protocolPointer(response.Outcome, outcomeV1),
	}
}

func goalV1(goal Goal) protocolv1.Goal {
	return protocolv1.Goal{Target: string(goal.Target), Direction: string(goal.Direction)}
}

func inspectionV1(inspection Inspection) protocolv1.Inspection {
	return protocolv1.Inspection{
		Plan: string(inspection.Plan),
		Assessment: protocolv1.ActionAssessment{
			Action: string(inspection.Assessment.Action), Effect: string(inspection.Assessment.Effect),
			Changed: inspection.Assessment.Changed, Recovery: string(inspection.Assessment.Recovery),
			Impacts:      protocolSlice(inspection.Assessment.Impacts, func(value domain.ActionImpact) string { return string(value) }),
			Consequences: slices.Clone(inspection.Assessment.Consequences),
		},
		Decisions: protocolSlice(inspection.Decisions, func(value RedactedDecision) protocolv1.RedactedDecision {
			return protocolv1.RedactedDecision{Resource: value.Resource, Scope: value.Scope, Decision: string(value.Decision), Result: value.Result}
		}),
		Blockers: protocolSlice(inspection.Blockers, func(value Blocker) protocolv1.Blocker {
			return protocolv1.Blocker{Code: string(value.Code), Resource: value.Resource, Message: value.Message, Retry: value.Retry}
		}),
		Resume:  protocolPointer(inspection.Resume, func(value TransactionID) string { return string(value) }),
		Outcome: protocolPointer(inspection.Outcome, outcomeV1),
	}
}

func inspectionFromV1(inspection protocolv1.Inspection) Inspection {
	return Inspection{
		Plan: PlanToken(inspection.Plan),
		Assessment: domain.ActionAssessment{
			Action: domain.ActionID(inspection.Assessment.Action), Effect: domain.ActionEffect(inspection.Assessment.Effect),
			Changed: inspection.Assessment.Changed, Recovery: domain.RecoveryClass(inspection.Assessment.Recovery),
			Impacts:      protocolSlice(inspection.Assessment.Impacts, func(value string) domain.ActionImpact { return domain.ActionImpact(value) }),
			Consequences: slices.Clone(inspection.Assessment.Consequences),
		},
		Decisions: protocolSlice(inspection.Decisions, func(value protocolv1.RedactedDecision) RedactedDecision {
			return RedactedDecision{Resource: value.Resource, Scope: value.Scope, Decision: Decision(value.Decision), Result: value.Result}
		}),
		Blockers: protocolSlice(inspection.Blockers, func(value protocolv1.Blocker) Blocker {
			return Blocker{Code: OutcomeCode(value.Code), Resource: value.Resource, Message: value.Message, Retry: value.Retry}
		}),
		Resume:  protocolPointer(inspection.Resume, func(value string) TransactionID { return TransactionID(value) }),
		Outcome: protocolPointer(inspection.Outcome, outcomeFromV1),
	}
}

func outcomeV1(outcome Outcome) protocolv1.Outcome {
	return protocolv1.Outcome{
		Status: string(outcome.Status), ReachedGoal: outcome.ReachedGoal,
		Active: string(outcome.Active), Target: string(outcome.Target),
		Previous: protocolPointer(outcome.Previous, func(value ReleaseID) string { return string(value) }),
		Code:     string(outcome.Code), Message: outcome.Message, Retry: outcome.Retry,
		Transaction: protocolPointer(outcome.Transaction, func(value TransactionID) string { return string(value) }),
		Warnings:    slices.Clone(outcome.Warnings),
	}
}

func outcomeFromV1(outcome protocolv1.Outcome) Outcome {
	return Outcome{
		Status: PublicStatus(outcome.Status), ReachedGoal: outcome.ReachedGoal,
		Active: ReleaseID(outcome.Active), Target: ReleaseID(outcome.Target),
		Previous: protocolPointer(outcome.Previous, func(value string) ReleaseID { return ReleaseID(value) }),
		Code:     OutcomeCode(outcome.Code), Message: outcome.Message, Retry: outcome.Retry,
		Transaction: protocolPointer(outcome.Transaction, func(value string) TransactionID { return TransactionID(value) }),
		Warnings:    slices.Clone(outcome.Warnings),
	}
}

func sourceIngressV1(source SourceIngressRequest) protocolv1.SourceIngressRequest {
	return protocolv1.SourceIngressRequest{
		SchemaVersion: source.SchemaVersion, Kind: string(source.Kind), SourceRoot: source.SourceRoot,
		DataHome: source.DataHome, BinDir: source.BinDir, RC: source.RC, LoginRC: source.LoginRC,
	}
}

func sourceIngressFromV1(source protocolv1.SourceIngressRequest) SourceIngressRequest {
	return SourceIngressRequest{
		SchemaVersion: source.SchemaVersion, Kind: SourceIngressRequestKind(source.Kind), SourceRoot: source.SourceRoot,
		DataHome: source.DataHome, BinDir: source.BinDir, RC: source.RC, LoginRC: source.LoginRC,
	}
}

func replacementV1(replacement JournalReplacement) protocolv1.JournalReplacement {
	return protocolv1.JournalReplacement{
		Transaction: string(replacement.Transaction), Fingerprint: string(replacement.Fingerprint),
		Reason: string(replacement.Reason), SourceVersion: replacement.SourceVersion,
	}
}

func replacementFromV1(replacement protocolv1.JournalReplacement) JournalReplacement {
	return JournalReplacement{
		Transaction: TransactionID(replacement.Transaction), Fingerprint: Fingerprint(replacement.Fingerprint),
		Reason: JournalReplacementReason(replacement.Reason), SourceVersion: replacement.SourceVersion,
	}
}

func protocolPointer[A, B any](value *A, convert func(A) B) *B {
	if value == nil {
		return nil
	}
	result := convert(*value)
	return &result
}

func protocolSlice[A, B any](values []A, convert func(A) B) []B {
	if values == nil {
		return nil
	}
	result := make([]B, len(values))
	for index, value := range values {
		result[index] = convert(value)
	}
	return result
}
