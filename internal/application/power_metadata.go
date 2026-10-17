package application

import (
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

const (
	powerManagedKey     = "user.subyard.managed"
	powerInitializedKey = "user.subyard.initialized"
	powerDesiredKey     = "user.subyard.desired_power"
	powerYardNameKey    = "user.subyard.name"
	powerBridgeKey      = "user.subyard.bridge"
	powerAutostartKey   = "boot.autostart"
)

type metadataFieldState uint8

const (
	fieldAbsent metadataFieldState = iota
	fieldEmpty
	fieldValid
	fieldInvalid
)

type metadataField[T any] struct {
	State metadataFieldState
	Raw   string
	Value T
}

type powerMetadata struct {
	Managed     metadataField[bool]
	Initialized metadataField[bool]
	Desired     metadataField[domain.DesiredPower]
	YardName    metadataField[string]
	Bridge      metadataField[string]
	Autostart   metadataField[bool]
}

// parsePowerMetadata reads only effective config. Syntax is independent of phase policy.
func parsePowerMetadata(instance ports.InstanceInfo) powerMetadata {
	return powerMetadata{
		Managed:     parseMetadataField(instance, powerManagedKey, parsePowerBoolean),
		Initialized: parseMetadataField(instance, powerInitializedKey, parsePowerBoolean),
		Desired: parseMetadataField(instance, powerDesiredKey, func(raw string) (domain.DesiredPower, bool) {
			value := domain.DesiredPower(raw)
			return value, value == domain.PowerRunning || value == domain.PowerStopped
		}),
		YardName:  parseMetadataField(instance, powerYardNameKey, parsePowerOpaque),
		Bridge:    parseMetadataField(instance, powerBridgeKey, parsePowerOpaque),
		Autostart: parseMetadataField(instance, powerAutostartKey, parsePowerBoolean),
	}
}

func parseMetadataField[T any](instance ports.InstanceInfo, key string, parse func(string) (T, bool)) metadataField[T] {
	raw, present := instance.EffectiveConfig(key)
	field := metadataField[T]{Raw: raw}
	if !present {
		return field
	}
	if raw == "" {
		field.State = fieldEmpty
		return field
	}
	value, valid := parse(raw)
	field.State = fieldInvalid
	if valid {
		field.State, field.Value = fieldValid, value
	}
	return field
}

func parsePowerBoolean(raw string) (bool, bool) {
	return raw == "true", raw == "true" || raw == "false"
}
func parsePowerOpaque(raw string) (string, bool) { return raw, true }

// InstanceIntentionallyStopped is the runtime shortcut; identity and autostart are irrelevant here.
func InstanceIntentionallyStopped(instance ports.InstanceInfo) bool {
	metadata := parsePowerMetadata(instance)
	return strings.EqualFold(instance.Status, "stopped") &&
		metadata.Managed.State == fieldValid && metadata.Managed.Value &&
		metadata.Initialized.State == fieldValid && metadata.Initialized.Value &&
		metadata.Desired.State == fieldValid && metadata.Desired.Value == domain.PowerStopped
}

// PowerMetadataReadyForFinalize checks the runtime's metadata prerequisite, ignoring yard name.
func PowerMetadataReadyForFinalize(instance ports.InstanceInfo, bridge string) bool {
	metadata := parsePowerMetadata(instance)
	return metadata.Managed.State == fieldValid && metadata.Managed.Value &&
		metadata.Initialized.State == fieldValid && metadata.Initialized.Value &&
		metadata.Desired.State == fieldValid && metadata.Bridge.Raw == bridge &&
		metadata.Autostart.State == fieldValid && !metadata.Autostart.Value
}

// ProjectHookStoppedIntentPreserved checks intent after writes, while initialization may be fenced.
func ProjectHookStoppedIntentPreserved(instance ports.InstanceInfo) bool {
	metadata := parsePowerMetadata(instance)
	return metadata.Managed.State == fieldValid && metadata.Managed.Value &&
		metadata.Desired.State == fieldValid && metadata.Desired.Value == domain.PowerStopped
}
