package releasetransition

import "encoding/json"

// Frozen V1 syntax only. These fields mirror migration.transaction; the Rust
// owner checks history, paths, phases and import eligibility. This package must
// not import migration (which consumes the frozen release DTOs).
type legacyV1Transaction struct {
	SchemaVersion int                 `json:"schemaVersion"`
	FromLayout    int                 `json:"fromLayout"`
	ToLayout      int                 `json:"toLayout"`
	FromRuntime   string              `json:"fromRuntime,omitempty"`
	ToRelease     string              `json:"toRelease"`
	ToRuntime     string              `json:"toRuntime,omitempty"`
	Phase         string              `json:"phase"`
	Migrations    []string            `json:"migrations"`
	Entries       []legacyV1Entry     `json:"entries,omitempty"`
	Operations    []legacyV1Operation `json:"operations,omitempty"`
	RollbackOps   bool                `json:"rollbackOperations,omitempty"`
}

type legacyV1Entry struct {
	MigrationID string `json:"migrationId"`
	Scope       string `json:"scope"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Consumer    string `json:"consumer"`
	Recovery    string `json:"recovery"`
	Digest      string `json:"sha256,omitempty"`
	Mode        uint32 `json:"mode,omitempty"`
}

type legacyV1Operation struct {
	MigrationID string `json:"migrationId"`
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	Before      string `json:"before,omitempty"`
	Phase       string `json:"phase,omitempty"`
}

// The response's eligibility is checked by Rust. Raw result fields preserve
// presence without calling RecoveryProcessResponse.UnmarshalJSON/Validate.
type sourceCapabilitySyntax struct {
	SchemaVersion                 int                   `json:"schemaVersion"`
	ActivationReconciliationOwned bool                  `json:"activationReconciliationOwned"`
	Capabilities                  *RecoveryCapabilities `json:"capabilities,omitempty"`
	ActivationOwners              []string              `json:"activationOwners,omitempty"`
	Inspection                    *json.RawMessage      `json:"inspection,omitempty"`
	Outcome                       *json.RawMessage      `json:"outcome,omitempty"`
}

type legacyV1Registry struct {
	SchemaVersion int                  `json:"schemaVersion"`
	MinimumLayout int                  `json:"minimumLayout"`
	CurrentLayout int                  `json:"currentLayout"`
	Migrations    []legacyV1Definition `json:"migrations"`
}
type legacyV1Definition struct {
	ID             string                      `json:"id"`
	FromLayout     int                         `json:"fromLayout"`
	ToLayout       int                         `json:"toLayout"`
	Resources      []string                    `json:"resources"`
	FinalizePolicy string                      `json:"finalizePolicy"`
	RollbackPolicy string                      `json:"rollbackPolicy"`
	Moves          []legacyV1Move              `json:"moves,omitempty"`
	Operations     []legacyV1RegistryOperation `json:"operations,omitempty"`
}
type legacyV1Move struct {
	Scope       string `json:"scope"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Consumer    string `json:"consumer"`
}
type legacyV1RegistryOperation struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}
type legacyV1RouteConsumers struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Active        bool                    `json:"active"`
	Consumers     []legacyV1RouteConsumer `json:"consumers"`
}
type legacyV1RouteConsumer struct {
	Project  string `json:"project"`
	Instance string `json:"instance"`
	Yard     string `json:"yard"`
	Mounted  bool   `json:"mounted"`
}
