package migration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

const registrySchemaVersion = 1

var migrationIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Registry is the declarative, release-owned migration contract.
type Registry struct {
	SchemaVersion int          `json:"schemaVersion"`
	MinimumLayout int          `json:"minimumLayout"`
	CurrentLayout int          `json:"currentLayout"`
	Migrations    []Definition `json:"migrations"`
}

// Definition describes one ordered layout transition. Legacy definitions move
// explicitly named files. New definitions select versioned engine-owned
// operations; neither form can name arbitrary release code.
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
	OperationKindTestYardOwnerV1                = "test-yard-owner-v1"
	OperationKindTestYardRouteConsumersV1       = "test-yard-route-consumers-v1"
	OperationKindTestVMBrokerRuntimeV1          = "test-vm-broker-runtime-v1"
	OperationKindPowerReconcilerRuntimeV1       = "power-reconciler-runtime-v1"
	OperationKindPowerReconcilerSystemdCompatV1 = "power-reconciler-systemd-compat-v1"

	fileFinalizePolicy    = "remove-source-after-active-verify"
	fileRollbackPolicy    = "restore-recovery-before-runtime-swap"
	orderedFinalizePolicy = "ordered-after-active-verify"
	orderedRollbackPolicy = "reverse-before-runtime-swap"
)

// LoadRegistry reads and validates a release-owned migration registry.
func LoadRegistry(path string) (Registry, error) {
	file, err := os.Open(path)
	if err != nil {
		return Registry{}, fmt.Errorf("open migration registry %q: %w", path, err)
	}
	defer file.Close()

	var registry Registry
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registry); err != nil {
		return Registry{}, fmt.Errorf("decode migration registry %q: %w", path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Registry{}, fmt.Errorf("decode migration registry %q: %w", path, err)
	}
	if err := registry.Validate(); err != nil {
		return Registry{}, fmt.Errorf("validate migration registry %q: %w", path, err)
	}
	return registry, nil
}

// Validate rejects ambiguous, unsafe, or non-contiguous migration contracts.
func (registry Registry) Validate() error {
	if registry.SchemaVersion != registrySchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d", registry.SchemaVersion)
	}
	if registry.MinimumLayout < 1 {
		return fmt.Errorf("minimumLayout must be positive")
	}
	if registry.CurrentLayout < registry.MinimumLayout {
		return fmt.Errorf("currentLayout %d is below minimumLayout %d", registry.CurrentLayout, registry.MinimumLayout)
	}

	ids := make(map[string]struct{}, len(registry.Migrations))
	expectedFrom := registry.MinimumLayout
	for index, definition := range registry.Migrations {
		if !migrationIDPattern.MatchString(definition.ID) {
			return fmt.Errorf("migration %d has invalid id %q", index, definition.ID)
		}
		if _, exists := ids[definition.ID]; exists {
			return fmt.Errorf("duplicate migration id %q", definition.ID)
		}
		ids[definition.ID] = struct{}{}
		if definition.FromLayout != expectedFrom {
			return fmt.Errorf(
				"migration %q starts at layout %d; expected %d",
				definition.ID,
				definition.FromLayout,
				expectedFrom,
			)
		}
		if definition.ToLayout != definition.FromLayout+1 {
			return fmt.Errorf("migration %q must advance exactly one layout", definition.ID)
		}
		if len(definition.Resources) == 0 {
			return fmt.Errorf("migration %q has no logical resources", definition.ID)
		}
		for _, resource := range definition.Resources {
			if !migrationIDPattern.MatchString(resource) {
				return fmt.Errorf("migration %q has invalid logical resource %q", definition.ID, resource)
			}
		}
		if err := validateDefinitionOperations(definition); err != nil {
			return fmt.Errorf("migration %q: %w", definition.ID, err)
		}
		expectedFrom = definition.ToLayout
	}
	if expectedFrom != registry.CurrentLayout {
		return fmt.Errorf(
			"migration chain ends at layout %d; currentLayout is %d",
			expectedFrom,
			registry.CurrentLayout,
		)
	}
	return nil
}

func validateDefinitionOperations(definition Definition) error {
	if len(definition.Moves) == 0 && len(definition.Operations) == 0 {
		return errors.New("has no moves or typed operations")
	}
	if len(definition.Operations) == 0 {
		if definition.FinalizePolicy != fileFinalizePolicy {
			return fmt.Errorf("unsupported finalize policy %q", definition.FinalizePolicy)
		}
		if definition.RollbackPolicy != fileRollbackPolicy {
			return fmt.Errorf("unsupported rollback policy %q", definition.RollbackPolicy)
		}
	} else {
		if definition.FinalizePolicy != orderedFinalizePolicy {
			return fmt.Errorf("unsupported finalize policy %q", definition.FinalizePolicy)
		}
		if definition.RollbackPolicy != orderedRollbackPolicy {
			return fmt.Errorf("unsupported rollback policy %q", definition.RollbackPolicy)
		}
	}
	for moveIndex, move := range definition.Moves {
		if err := validateMove(move); err != nil {
			return fmt.Errorf("move %d: %w", moveIndex, err)
		}
	}
	if len(definition.Operations) == 0 {
		return nil
	}
	ids := make(map[string]struct{}, len(definition.Operations))
	for index, operation := range definition.Operations {
		if !migrationIDPattern.MatchString(operation.ID) {
			return fmt.Errorf("operation %d has invalid id %q", index, operation.ID)
		}
		if _, exists := ids[operation.ID]; exists {
			return fmt.Errorf("duplicate operation id %q", operation.ID)
		}
		ids[operation.ID] = struct{}{}
		switch operation.Kind {
		case OperationKindTestYardOwnerV1,
			OperationKindTestYardRouteConsumersV1,
			OperationKindTestVMBrokerRuntimeV1,
			OperationKindPowerReconcilerRuntimeV1,
			OperationKindPowerReconcilerSystemdCompatV1:
		default:
			return fmt.Errorf("operation %q has unsupported kind %q", operation.ID, operation.Kind)
		}
	}
	return nil
}

// Path returns the exact ordered transition from an observed layout.
func (registry Registry) Path(fromLayout int) ([]Definition, error) {
	if fromLayout < registry.MinimumLayout {
		return nil, fmt.Errorf(
			"layout %d is older than supported minimum %d",
			fromLayout,
			registry.MinimumLayout,
		)
	}
	if fromLayout > registry.CurrentLayout {
		return nil, fmt.Errorf(
			"layout %d is newer than release layout %d",
			fromLayout,
			registry.CurrentLayout,
		)
	}
	if fromLayout == registry.CurrentLayout {
		return nil, nil
	}

	var path []Definition
	layout := fromLayout
	for _, definition := range registry.Migrations {
		if definition.FromLayout < layout {
			continue
		}
		if definition.FromLayout != layout {
			return nil, fmt.Errorf("no migration registered from layout %d", layout)
		}
		path = append(path, definition)
		layout = definition.ToLayout
		if layout == registry.CurrentLayout {
			return path, nil
		}
	}
	return nil, fmt.Errorf("migration path stops at layout %d", layout)
}

func validateMove(move Move) error {
	switch move.Scope {
	case "config-home", "data-home":
	default:
		return fmt.Errorf("unsupported scope %q", move.Scope)
	}
	if !safeRelativePath(move.Source) {
		return fmt.Errorf("unsafe source %q", move.Source)
	}
	if !safeRelativePath(move.Destination) {
		return fmt.Errorf("unsafe destination %q", move.Destination)
	}
	if filepath.Clean(move.Source) == filepath.Clean(move.Destination) {
		return fmt.Errorf("source and destination are identical")
	}
	switch move.Consumer {
	case "regular-file", "assignments", "json":
	default:
		return fmt.Errorf("unsupported consumer %q", move.Consumer)
	}
	return nil
}

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
