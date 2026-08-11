package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

type ActionID string

type ActionEffect string

const (
	ActionRead         ActionEffect = "read"
	ActionSession      ActionEffect = "session"
	ActionBoundedWrite ActionEffect = "bounded-write"
	ActionMutation     ActionEffect = "mutation"
	ActionDestruction  ActionEffect = "destruction"
)

type ActionImpact string

const (
	ImpactLocalMetadata  ActionImpact = "local-metadata"
	ImpactYardRuntime    ActionImpact = "yard-runtime"
	ImpactHostOS         ActionImpact = "host-os"
	ImpactHostNetwork    ActionImpact = "host-network"
	ImpactHostIncus      ActionImpact = "host-incus"
	ImpactSecurity       ActionImpact = "security"
	ImpactTrust          ActionImpact = "trust"
	ImpactAccess         ActionImpact = "access"
	ImpactExternalSystem ActionImpact = "external-system"
	ImpactSharedWorkload ActionImpact = "shared-workload"
	ImpactPersistentData ActionImpact = "persistent-data"
)

type RecoveryClass string

const (
	RecoveryNotNeeded    RecoveryClass = "not-needed"
	RecoveryReversible   RecoveryClass = "reversible"
	RecoveryRecreatable  RecoveryClass = "recreatable"
	RecoveryIrreversible RecoveryClass = "irreversible"
)

type ActionConfirmationPolicy string

const (
	ActionConfirmationNever            ActionConfirmationPolicy = "never"
	ActionConfirmationPromptDefaultYes ActionConfirmationPolicy = "prompt-default-yes"
	ActionConfirmationPromptDefaultNo  ActionConfirmationPolicy = "prompt-default-no"
)

type ConfirmationDefault string

const (
	ConfirmationDefaultYes ConfirmationDefault = "yes"
	ConfirmationDefaultNo  ConfirmationDefault = "no"
)

type ActionAssessment struct {
	Action       ActionID       `json:"action"`
	Effect       ActionEffect   `json:"effect"`
	Changed      bool           `json:"changed"`
	Impacts      []ActionImpact `json:"impacts,omitempty"`
	Recovery     RecoveryClass  `json:"recovery"`
	Consequences []string       `json:"consequences,omitempty"`
}

type ConfirmationRequest struct {
	Summary      string              `json:"summary"`
	Consequences []string            `json:"consequences,omitempty"`
	Default      ConfirmationDefault `json:"default"`
}

type ActionDefinition struct {
	Action   ActionID
	Summary  string
	Effect   ActionEffect
	Impacts  []ActionImpact
	Recovery RecoveryClass
}

type ActionDelta struct {
	Changed      bool
	Consequences []string
}

func (assessment ActionAssessment) Clone() ActionAssessment {
	assessment.Impacts = slices.Clone(assessment.Impacts)
	assessment.Consequences = slices.Clone(assessment.Consequences)
	return assessment
}

func (request ConfirmationRequest) Clone() ConfirmationRequest {
	request.Consequences = slices.Clone(request.Consequences)
	return request
}

type ActionRegistry struct {
	definitions map[ActionID]ActionDefinition
}

const ActionPolicyInvalid = "action_policy_invalid"

var ErrActionPolicyInvalid = errors.New(ActionPolicyInvalid)

func ActionPolicyErrorClass(err error) string {
	if errors.Is(err, ErrActionPolicyInvalid) {
		return ActionPolicyInvalid
	}
	return ""
}

func NewActionRegistry(definitions []ActionDefinition) (*ActionRegistry, error) {
	registry := &ActionRegistry{definitions: make(map[ActionID]ActionDefinition, len(definitions))}
	for _, definition := range definitions {
		var err error
		definition, err = normalizeDefinition(definition)
		if err != nil {
			return nil, err
		}
		if _, exists := registry.definitions[definition.Action]; exists {
			return nil, actionPolicyInvalid("duplicate action definition %q", definition.Action)
		}
		registry.definitions[definition.Action] = definition
	}
	return registry, nil
}

// With returns a new registry containing the receiver's immutable definitions
// plus the supplied definitions. The receiver is never modified.
func (registry *ActionRegistry) With(definitions []ActionDefinition) (*ActionRegistry, error) {
	if registry == nil {
		return nil, actionPolicyInvalid("action registry is required")
	}
	combined := make([]ActionDefinition, 0, len(registry.definitions)+len(definitions))
	for _, definition := range registry.definitions {
		definition.Impacts = slices.Clone(definition.Impacts)
		combined = append(combined, definition)
	}
	combined = append(combined, definitions...)
	return NewActionRegistry(combined)
}

func (registry *ActionRegistry) Assess(action ActionID, delta ActionDelta) (ActionAssessment, error) {
	if registry == nil {
		return ActionAssessment{}, actionPolicyInvalid("action registry is required")
	}
	if !SafeID(string(action)) {
		return ActionAssessment{}, actionPolicyInvalid("invalid action ID %q", action)
	}
	definition, exists := registry.definitions[action]
	if !exists {
		return ActionAssessment{}, actionPolicyInvalid("unknown action %q", action)
	}
	consequences, err := normalizeConsequences(delta.Consequences)
	if err != nil {
		return ActionAssessment{}, err
	}
	if delta.Changed && isStateChanging(definition.Effect) && len(consequences) == 0 {
		return ActionAssessment{}, actionPolicyInvalid("changed action %q requires a consequence", action)
	}
	return ActionAssessment{
		Action:       definition.Action,
		Effect:       definition.Effect,
		Changed:      delta.Changed,
		Impacts:      slices.Clone(definition.Impacts),
		Recovery:     definition.Recovery,
		Consequences: consequences,
	}, nil
}

func (registry *ActionRegistry) Resolve(assessment ActionAssessment) (ActionConfirmationPolicy, *ConfirmationRequest, error) {
	if registry == nil {
		return "", nil, actionPolicyInvalid("action registry is required")
	}
	definition, exists := registry.definitions[assessment.Action]
	if !exists {
		return "", nil, actionPolicyInvalid("unknown action %q", assessment.Action)
	}
	if err := validateAssessment(definition, assessment); err != nil {
		return "", nil, err
	}
	if !assessment.Changed {
		return ActionConfirmationNever, nil, nil
	}
	switch definition.Effect {
	case ActionRead, ActionSession, ActionBoundedWrite:
		return ActionConfirmationNever, nil, nil
	}
	request := &ConfirmationRequest{
		Summary: definition.Summary, Consequences: slices.Clone(assessment.Consequences),
	}
	switch definition.Recovery {
	case RecoveryReversible, RecoveryRecreatable:
		request.Default = ConfirmationDefaultYes
		return ActionConfirmationPromptDefaultYes, request, nil
	case RecoveryIrreversible:
		request.Default = ConfirmationDefaultNo
		return ActionConfirmationPromptDefaultNo, request, nil
	default:
		return "", nil, actionPolicyInvalid("invalid recovery for action %q", assessment.Action)
	}
}

func normalizeDefinition(definition ActionDefinition) (ActionDefinition, error) {
	if !SafeID(string(definition.Action)) {
		return ActionDefinition{}, actionPolicyInvalid("invalid action ID %q", definition.Action)
	}
	if !safeText(definition.Summary) {
		return ActionDefinition{}, actionPolicyInvalid("invalid summary for action %q", definition.Action)
	}
	if !validActionEffect(definition.Effect) {
		return ActionDefinition{}, actionPolicyInvalid("invalid effect for action %q", definition.Action)
	}
	if !validRecoveryClass(definition.Recovery) {
		return ActionDefinition{}, actionPolicyInvalid("invalid recovery for action %q", definition.Action)
	}
	impacts, err := normalizeImpacts(definition.Impacts)
	if err != nil {
		return ActionDefinition{}, err
	}
	definition.Summary = strings.TrimSpace(definition.Summary)
	definition.Impacts = impacts
	if definition.Effect == ActionRead || definition.Effect == ActionSession || definition.Effect == ActionBoundedWrite {
		if definition.Recovery != RecoveryNotNeeded {
			return ActionDefinition{}, actionPolicyInvalid("effect %q requires not-needed recovery", definition.Effect)
		}
		return definition, nil
	}
	if len(definition.Impacts) == 0 {
		return ActionDefinition{}, actionPolicyInvalid("effect %q requires an impact", definition.Effect)
	}
	if definition.Recovery == RecoveryNotNeeded {
		return ActionDefinition{}, actionPolicyInvalid("effect %q requires a recovery class", definition.Effect)
	}
	return definition, nil
}

func validateAssessment(definition ActionDefinition, assessment ActionAssessment) error {
	if !SafeID(string(assessment.Action)) || assessment.Action != definition.Action ||
		assessment.Effect != definition.Effect || assessment.Recovery != definition.Recovery {
		return actionPolicyInvalid("assessment metadata does not match action %q", definition.Action)
	}
	impacts, err := normalizeImpacts(assessment.Impacts)
	if err != nil {
		return err
	}
	if !slices.Equal(impacts, definition.Impacts) {
		return actionPolicyInvalid("assessment impacts do not match action %q", definition.Action)
	}
	consequences, err := normalizeConsequences(assessment.Consequences)
	if err != nil {
		return err
	}
	if !slices.Equal(consequences, assessment.Consequences) {
		return actionPolicyInvalid("assessment consequences are not normalized")
	}
	if assessment.Changed && isStateChanging(assessment.Effect) && len(consequences) == 0 {
		return actionPolicyInvalid("changed action %q requires a consequence", assessment.Action)
	}
	return nil
}

func normalizeImpacts(impacts []ActionImpact) ([]ActionImpact, error) {
	normalized := slices.Clone(impacts)
	for _, impact := range normalized {
		if !validActionImpact(impact) {
			return nil, actionPolicyInvalid("invalid action impact %q", impact)
		}
	}
	slices.Sort(normalized)
	return slices.Compact(normalized), nil
}

func normalizeConsequences(consequences []string) ([]string, error) {
	normalized := make([]string, len(consequences))
	for index, consequence := range consequences {
		if !safeText(consequence) {
			return nil, actionPolicyInvalid("invalid action consequence")
		}
		normalized[index] = strings.TrimSpace(consequence)
	}
	return normalized, nil
}

func safeText(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

func isStateChanging(effect ActionEffect) bool {
	return effect == ActionMutation || effect == ActionDestruction
}

func validActionEffect(effect ActionEffect) bool {
	return effect == ActionRead || effect == ActionSession || effect == ActionBoundedWrite ||
		effect == ActionMutation || effect == ActionDestruction
}

func validActionImpact(impact ActionImpact) bool {
	switch impact {
	case ImpactLocalMetadata, ImpactYardRuntime, ImpactHostOS, ImpactHostNetwork, ImpactHostIncus,
		ImpactSecurity, ImpactTrust, ImpactAccess, ImpactExternalSystem, ImpactSharedWorkload, ImpactPersistentData:
		return true
	default:
		return false
	}
}

func validRecoveryClass(recovery RecoveryClass) bool {
	return recovery == RecoveryNotNeeded || recovery == RecoveryReversible ||
		recovery == RecoveryRecreatable || recovery == RecoveryIrreversible
}

func actionPolicyInvalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrActionPolicyInvalid, fmt.Sprintf(format, arguments...))
}
