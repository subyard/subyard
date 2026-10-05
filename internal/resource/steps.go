package resource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

// Step describes an approved effect, not a handler command or a new executor.
// Conditional effects may name earlier prerequisites and retain an exact target
// and desired postcondition even when their observation is not yet available.
type Step = domain.OperationStep

// PrepareResult retains the handler's private binding separately from public
// preview fields. Binding is a SHA-256 fingerprint, never a protected payload.
type PrepareResult struct {
	Schema     string
	Assessment domain.ActionAssessment
	Steps      []Step
	Binding    string
}

func (registry Registry) PrepareResult(actions *domain.ActionRegistry, resource, verb string, output []byte) (PrepareResult, error) {
	assessment, err := registry.AssessPrepareResult(actions, resource, verb, output)
	if err != nil {
		return PrepareResult{}, err
	}
	document, err := decodePrepareAssessment(output)
	if err != nil {
		return PrepareResult{}, err
	}
	result := PrepareResult{Schema: document.Schema, Assessment: assessment, Binding: document.Binding}
	if document.Steps != nil {
		result.Steps = *document.Steps
	}
	return result, nil
}

// VerifyRequest is a bounded read-only native-handler query after apply. It
// carries approved expected postconditions only: no stdin, lease, apply token,
// secret or arbitrary command. This descriptor does not advertise support by
// existing handlers; profiles must implement and validate verify explicitly.
const VerifyRequestSchema = "yard.resource-action-verify.v1"

type VerifyRequest struct {
	Schema string `json:"schema"`
	Steps  []Step `json:"steps"`
}

func validateStructuredAssessment(document prepareAssessmentDocument) error {
	if document.Schema == PrepareAssessmentSchema {
		if document.Steps != nil || document.Binding != "" {
			return resourcePlanInvalid("v1 does not support structured steps or binding")
		}
		return nil
	}
	if document.Steps == nil {
		return resourcePlanInvalid("v2 requires structured steps")
	}
	if document.Binding != "" && (len(document.Binding) != 64 || !validHexDigest(document.Binding) || strings.ToLower(document.Binding) != document.Binding) {
		return resourcePlanInvalid("binding must be a lowercase SHA-256 digest")
	}
	if len(*document.Steps) > 64 {
		return resourcePlanInvalid("too many structured steps")
	}
	seen := make(map[string]bool)
	changed := false
	for _, step := range *document.Steps {
		if !domain.SafeID(step.ID) || seen[step.ID] {
			return resourcePlanInvalid("invalid or duplicate step ID")
		}
		for _, value := range []string{step.Target, step.Observed, step.Desired, step.Verify} {
			if err := validateConsequence(value); err != nil {
				return err
			}
		}
		if step.Consequence != "" {
			if err := validateConsequence(step.Consequence); err != nil {
				return err
			}
		}
		if len(step.Preconditions) > 64 || len(step.DependsOn) > 64 {
			return resourcePlanInvalid("too many step guards")
		}
		for _, guard := range step.Preconditions {
			if err := validateConsequence(guard); err != nil {
				return err
			}
		}
		dependencies := make(map[string]bool)
		for _, id := range step.DependsOn {
			if !seen[id] || dependencies[id] {
				return resourcePlanInvalid("step dependency must name a distinct earlier step")
			}
			dependencies[id] = true
		}
		switch step.Decision {
		case "apply":
			changed = true
		case "skip":
		case "conditional":
			changed = true
			if step.Observed != "unknown" || len(step.Preconditions) == 0 {
				return resourcePlanInvalid("conditional step requires unknown observation and guard")
			}
		default:
			return resourcePlanInvalid("invalid step decision")
		}
		if step.Decision != "conditional" && step.Observed == "unknown" {
			return resourcePlanInvalid("unknown observation requires conditional decision")
		}
		seen[step.ID] = true
	}
	if document.Changed != nil && *document.Changed != changed {
		return resourcePlanInvalid("changed does not match structured steps")
	}
	return nil
}

// JSON duplicate keys are rejected at every depth, including objects in arrays.
func rejectRecursiveDuplicates(output []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(output))
	var read func() error
	read = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		seen := make(map[string]bool)
		for decoder.More() {
			if delimiter == '{' {
				token, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := token.(string)
				if !ok {
					return fmt.Errorf("invalid field")
				}
				folded := strings.ToLower(name)
				if seen[folded] {
					return fmt.Errorf("duplicate field %q", name)
				}
				seen[folded] = true
			}
			if err := read(); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	if err := read(); err != nil {
		return resourcePlanInvalid("decode assessment: %v", err)
	}
	return nil
}
