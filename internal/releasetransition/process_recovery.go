package releasetransition

import (
	"encoding/json"
	"slices"

	protocolv1 "github.com/Subyard/Subyard/internal/releasetransition/protocol/v1"
)

const ProcessRecoverySchemaV2 = 2

type RecoveryProcessMode string

const (
	RecoveryProcessCapabilities RecoveryProcessMode = "capabilities"
	RecoveryProcessInspect      RecoveryProcessMode = "inspect"
	RecoveryProcessConverge     RecoveryProcessMode = "converge"
)

// RecoveryCapabilities is explicitly negotiated with the sealed owner before
// using the separate recovery contract. Semver alone is not support evidence.
type RecoveryCapabilities struct {
	Contract      string `json:"contract"`
	ProcessSchema int    `json:"processSchema"`
	JournalSchema int    `json:"journalSchema"`
	ReceiptSchema int    `json:"receiptSchema"`
}

func ActivationOnlyRecoveryCapabilities() RecoveryCapabilities {
	return RecoveryCapabilities{ActivationOnlyRecoveryContractV1, ProcessRecoverySchemaV2, JournalSchemaV2, RecoveryReceiptSchemaV1}
}

func (capabilities RecoveryCapabilities) Validate() error {
	if capabilities != ActivationOnlyRecoveryCapabilities() {
		return invalid("unsupported activation recovery capabilities")
	}
	return nil
}

// RecoveryProcessRequest lives beside the frozen ordinary V1 process request.
// It cannot select rollback, ingress, a replacement reason or arbitrary policy.
type RecoveryProcessRequest struct {
	SchemaVersion       int                            `json:"schemaVersion"`
	Mode                RecoveryProcessMode            `json:"mode"`
	RuntimeRoot         string                         `json:"runtimeRoot,omitempty"`
	ConfigHome          string                         `json:"configHome,omitempty"`
	Yard                string                         `json:"yard,omitempty"`
	Target              ReleaseID                      `json:"target,omitempty"`
	Direction           Direction                      `json:"direction,omitempty"`
	ArtifactDigest      Fingerprint                    `json:"artifactDigest,omitempty"`
	RegistryDigest      Fingerprint                    `json:"registryDigest,omitempty"`
	InheritedSettingIDs []string                       `json:"inheritedSettingIds,omitempty"`
	Recovery            *ActivationOnlyRecoveryRequest `json:"recovery,omitempty"`
	Execution           *Execution                     `json:"execution,omitempty"`
}

func (request RecoveryProcessRequest) TransitionRequest() ProcessRequest {
	return ProcessRequest{SchemaVersion: ProcessProtocolSchemaV1, Mode: ProcessMode(request.Mode),
		RuntimeRoot: request.RuntimeRoot, ConfigHome: request.ConfigHome, Yard: request.Yard,
		Target: request.Target, Direction: request.Direction, ArtifactDigest: request.ArtifactDigest,
		RegistryDigest: request.RegistryDigest, InheritedSettingIDs: slices.Clone(request.InheritedSettingIDs),
		Execution: request.Execution}
}

func (request RecoveryProcessRequest) Validate() error {
	if request.SchemaVersion != ProcessRecoverySchemaV2 {
		return invalid("unsupported recovery process schema")
	}
	if request.Mode == RecoveryProcessCapabilities {
		if request.RuntimeRoot != "" || request.ConfigHome != "" || request.Yard != "" || request.Target != "" ||
			request.Direction != "" || request.ArtifactDigest != "" || request.RegistryDigest != "" ||
			len(request.InheritedSettingIDs) != 0 || request.Recovery != nil || request.Execution != nil {
			return invalid("recovery capability probe carries transition inputs")
		}
		return nil
	}
	if request.Mode != RecoveryProcessInspect && request.Mode != RecoveryProcessConverge {
		return invalid("unknown recovery process mode")
	}
	if request.Direction != DirectionActivateTarget || request.Recovery == nil {
		return invalid("recovery process requires a selected forward activation-only journal")
	}
	for _, root := range []string{request.RuntimeRoot, request.ConfigHome} {
		if err := validateSourceIngressRolePath(root); err != nil {
			return invalid("recovery roots must be absolute non-root paths")
		}
	}
	if err := request.Recovery.Validate(); err != nil {
		return err
	}
	if err := validateFingerprint(request.RegistryDigest, "recovery registry digest"); err != nil {
		return err
	}
	// Reuse the bounded ordinary contract for shared roots, enums and execution.
	_, err := json.Marshal(request.TransitionRequest())
	if err != nil {
		return err
	}
	if request.Execution != nil {
		if err := validatePlanToken(request.Execution.Plan); err != nil {
			return err
		}
		if err := validateText(string(request.Execution.Authorization), "recovery authorization", maxDiagnosticText, false); err != nil {
			return err
		}
	}
	return nil
}

type RecoveryProcessResponse struct {
	SchemaVersion                 int                   `json:"schemaVersion"`
	ActivationReconciliationOwned bool                  `json:"activationReconciliationOwned"`
	Capabilities                  *RecoveryCapabilities `json:"capabilities,omitempty"`
	Inspection                    *Inspection           `json:"inspection,omitempty"`
	Outcome                       *Outcome              `json:"outcome,omitempty"`
}

type recoveryProcessResponseWire struct {
	SchemaVersion                 int                    `json:"schemaVersion"`
	ActivationReconciliationOwned bool                   `json:"activationReconciliationOwned"`
	Capabilities                  *RecoveryCapabilities  `json:"capabilities,omitempty"`
	Inspection                    *protocolv1.Inspection `json:"inspection,omitempty"`
	Outcome                       *protocolv1.Outcome    `json:"outcome,omitempty"`
}

// V2 adds only the explicit recovery/capability seam; public results still
// project onto the frozen V1 vocabulary instead of exposing future internals.
func (response RecoveryProcessResponse) MarshalJSON() ([]byte, error) {
	if err := response.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(recoveryProcessResponseWire{
		SchemaVersion: response.SchemaVersion, ActivationReconciliationOwned: response.ActivationReconciliationOwned,
		Capabilities: response.Capabilities, Inspection: protocolPointer(response.Inspection, inspectionV1),
		Outcome: protocolPointer(response.Outcome, outcomeV1),
	})
}

func (response *RecoveryProcessResponse) UnmarshalJSON(payload []byte) error {
	var wire recoveryProcessResponseWire
	if err := decodeBoundedRecord(payload, MaxProtectedRecordBytes, &wire); err != nil {
		return err
	}
	parsed := RecoveryProcessResponse{
		SchemaVersion: wire.SchemaVersion, ActivationReconciliationOwned: wire.ActivationReconciliationOwned,
		Capabilities: wire.Capabilities, Inspection: protocolPointer(wire.Inspection, inspectionFromV1),
		Outcome: protocolPointer(wire.Outcome, outcomeFromV1),
	}
	if err := parsed.Validate(); err != nil {
		return err
	}
	*response = parsed
	return nil
}

func (response RecoveryProcessResponse) Validate() error {
	if response.SchemaVersion != ProcessRecoverySchemaV2 {
		return invalid("unsupported recovery response schema")
	}
	if response.Capabilities != nil {
		if response.Inspection != nil || response.Outcome != nil || response.ActivationReconciliationOwned {
			return invalid("recovery capability response carries transition results")
		}
		return response.Capabilities.Validate()
	}
	_, err := json.Marshal(ProcessResponse{SchemaVersion: ProcessProtocolSchemaV1,
		ActivationReconciliationOwned: response.ActivationReconciliationOwned,
		Inspection:                    response.Inspection, Outcome: response.Outcome})
	return err
}

func ParseRecoveryProcessRequest(payload []byte) (RecoveryProcessRequest, error) {
	var request RecoveryProcessRequest
	if err := decodeBoundedRecord(payload, MaxProtectedRecordBytes, &request); err != nil {
		return RecoveryProcessRequest{}, err
	}
	return request, request.Validate()
}

func MarshalRecoveryProcessRequest(request RecoveryProcessRequest) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

func ParseRecoveryProcessResponse(payload []byte) (RecoveryProcessResponse, error) {
	var response RecoveryProcessResponse
	if err := decodeBoundedRecord(payload, MaxProtectedRecordBytes, &response); err != nil {
		return RecoveryProcessResponse{}, err
	}
	return response, response.Validate()
}

func MarshalRecoveryProcessResponse(response RecoveryProcessResponse) ([]byte, error) {
	if err := response.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(response)
	if err == nil && len(payload) > MaxProtectedRecordBytes {
		return nil, invalid("recovery process response is too large")
	}
	return payload, err
}
