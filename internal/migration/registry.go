package migration

import (
	"encoding/json"
	"fmt"
	"io"
)

const registrySchemaVersion = 1

// Registry decodes only retained V1 evidence. It supplies no executable path.
type Registry struct {
	SchemaVersion int          `json:"schemaVersion"`
	MinimumLayout int          `json:"minimumLayout"`
	CurrentLayout int          `json:"currentLayout"`
	Migrations    []Definition `json:"migrations"`
}

// Definition preserves the historical registry encoding and digest. The bound
// reader admits only the two exact published owner/broker contracts.
type Definition struct {
	ID             string      `json:"id"`
	FromLayout     int         `json:"fromLayout"`
	ToLayout       int         `json:"toLayout"`
	Resources      []string    `json:"resources"`
	FinalizePolicy string      `json:"finalizePolicy"`
	RollbackPolicy string      `json:"rollbackPolicy"`
	Moves          []Move      `json:"moves,omitempty"`
	Operations     []Operation `json:"operations,omitempty"`
}

// Move describes one file relocation beneath an allowlisted operator root.
type Move struct {
	Scope       string `json:"scope"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Consumer    string `json:"consumer"`
}

// Operation selects a versioned handler compiled into the candidate engine.
// It intentionally has no executable, arguments, paths or arbitrary payload.
type Operation struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

const (
	OperationKindTestYardOwnerV1          = "test-yard-owner-v1"
	OperationKindTestYardRouteConsumersV1 = "test-yard-route-consumers-v1"
	OperationKindTestVMBrokerRuntimeV1    = "test-vm-broker-runtime-v1"

	orderedFinalizePolicy = "ordered-after-active-verify"
	orderedRollbackPolicy = "reverse-before-runtime-swap"
)

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("unexpected trailing JSON value")
	}
	return err
}
