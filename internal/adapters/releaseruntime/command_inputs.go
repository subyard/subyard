package releaseruntime

import (
	"maps"
	"slices"
	"sort"
	"strings"
)

// CommandInputs is the immutable original command environment, before loading
// yard settings. Its values and inherited setting IDs share one snapshot.
// Effective configuration maps cannot be used as release command inputs.
type CommandInputs struct {
	values              map[string]string
	inheritedSettingIDs []string
}

// CaptureCommandInputs belongs at command ingress. isSetting recognizes canonical
// setting names; legacy names remain exact process overrides without becoming IDs.
func CaptureCommandInputs(environment []string, isSetting func(string) bool) CommandInputs {
	inputs := CommandInputs{values: make(map[string]string)}
	for _, assignment := range environment {
		name, value, ok := strings.Cut(assignment, "=")
		if ok {
			inputs.values[name] = value
		}
	}
	delete(inputs.values, "SUBYARD_SUDO_PREAUTHORIZED")
	if isSetting != nil {
		for name := range inputs.values {
			if isSetting(name) {
				inputs.inheritedSettingIDs = append(inputs.inheritedSettingIDs, name)
			}
		}
		sort.Strings(inputs.inheritedSettingIDs)
	}
	return inputs
}

func (inputs CommandInputs) InheritedSettingIDs() []string {
	return slices.Clone(inputs.inheritedSettingIDs)
}

// RuntimeContext contains resolved installation paths and operation identity,
// never effective yard settings or a second source of command overrides.
type RuntimeContext struct {
	OperatorHome string
	DataHome     string
	ConfigHome   string
	RuntimeRoot  string
	OperationID  string
}

func (inputs CommandInputs) environment(context RuntimeContext) map[string]string {
	values := maps.Clone(inputs.values)
	if values == nil {
		values = make(map[string]string)
	}
	for name, value := range map[string]string{
		"SUBYARD_OPERATOR_HOME": context.OperatorHome,
		"SUBYARD_HOME":          context.DataHome,
		"SUBYARD_CONFIG_HOME":   context.ConfigHome,
		"YARD_RUNTIME_ROOT":     context.RuntimeRoot,
		"SUBYARD_OPERATION_ID":  context.OperationID,
	} {
		if value != "" {
			values[name] = value
		}
	}
	return values
}

func (inputs CommandInputs) value(name string, context RuntimeContext) string {
	var value string
	switch name {
	case "SUBYARD_OPERATOR_HOME":
		value = context.OperatorHome
	case "SUBYARD_HOME":
		value = context.DataHome
	case "SUBYARD_CONFIG_HOME":
		value = context.ConfigHome
	case "YARD_RUNTIME_ROOT":
		value = context.RuntimeRoot
	case "SUBYARD_OPERATION_ID":
		value = context.OperationID
	}
	if value != "" {
		return value
	}
	return inputs.values[name]
}
