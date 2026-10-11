package reconcileruntime

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

const lifecycleDiagnosticPrefix = "subyard-lifecycle-diagnostic-v1 "

var lifecycleReasons = map[string]string{
	"native-unavailable":  "native dependency or observation unavailable",
	"invalid-schema":      "invalid required native schema",
	"ambiguous-ownership": "native ownership is ambiguous; resources preserved",
	"foreign-ownership":   "native resources have foreign ownership; resources preserved",
	"observation-changed": "approved observation changed before effects",
	"convergence-failed":  "native lifecycle did not converge",
	"operation-failed":    "native lifecycle operation failed",
}

type integrationLifecycleError struct {
	cause          error
	message, retry string
	conflict       bool
}

func (failure *integrationLifecycleError) Error() string { return failure.message }
func (failure *integrationLifecycleError) Unwrap() error { return failure.cause }
func (failure *integrationLifecycleError) ActivationDiagnostic() (string, string) {
	return failure.message, failure.retry
}
func (failure *integrationLifecycleError) ActivationConflict() bool { return failure.conflict }

// Native output is private. Exactly one well-formed source-owned record may
// select a fixed reason; duplicate, malformed or contradictory records fall
// back safely, including historical handlers without this optional transport.
func lifecycleFailure(stderr []byte, phase string) (string, string) {
	reason := "operation-failed"
	reportedPhase := phase
	count := 0
	for _, line := range bytes.Split(stderr, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("subyard-lifecycle-diagnostic-v1")) {
			continue
		}
		count++
		if !bytes.HasPrefix(line, []byte(lifecycleDiagnosticPrefix)) {
			return phase, "operation-failed"
		}
		fields := strings.Split(strings.TrimPrefix(string(line), lifecycleDiagnosticPrefix), " ")
		if len(fields) != 2 || fields[0] != "inspect" && fields[0] != "apply" && fields[0] != "verify" {
			return phase, "operation-failed"
		}
		if phase != "apply" && fields[0] != "inspect" {
			return phase, "operation-failed"
		}
		if phase == "apply" {
			if fields[0] == "inspect" {
				return phase, "operation-failed"
			}
			reportedPhase = fields[0]
		}
		reason = fields[1]
		if _, ok := lifecycleReasons[reason]; !ok {
			return phase, "operation-failed"
		}
	}
	if count != 1 {
		return phase, "operation-failed"
	}
	return reportedPhase, reason
}

func (runtime Runtime) integrationDiagnostic(cause error, definition profile.IntegrationDefinition, phase, reason string) error {
	if cause == nil {
		return nil
	}
	var existing interface{ ActivationDiagnostic() (string, string) }
	if errors.As(cause, &existing) {
		return cause
	}
	if _, ok := lifecycleReasons[reason]; !ok {
		reason = "operation-failed"
	}
	if phase != "inspect" && phase != "apply" && phase != "verify" {
		phase = "inspect"
	}
	message, retry := "integration lifecycle "+phase, "yard integration status --json"
	validComponent := len(definition.Name) <= 63 && domain.SafeName(definition.Name)
	validYard := len(runtime.Yard.YardName) <= 63 && domain.SafeName(runtime.Yard.YardName)
	if validComponent {
		message = "integration " + definition.Name + " " + phase
		retry = "yard integration status " + definition.Name + " --json"
	}
	if validYard {
		message += " in yard " + runtime.Yard.YardName
		retry = "yard -Y " + runtime.Yard.YardName + " integration status --json"
		if validComponent {
			retry = "yard -Y " + runtime.Yard.YardName + " integration status " + definition.Name + " --json"
		}
	}
	if reason == "observation-changed" && !errors.Is(cause, domain.ErrPlanStale) {
		cause = fmt.Errorf("%w: %w", domain.ErrPlanStale, cause)
	}
	return &integrationLifecycleError{cause: cause, message: message + ": " + lifecycleReasons[reason], retry: retry, conflict: reason == "ambiguous-ownership" || reason == "foreign-ownership"}
}

// Attribute rejected plans using owner-private evidence. This does not decide
// whether a plan is stale or relax the existing convergence rules.
func (runtime Runtime) integrationPlanDiagnostic(cause error, approved, fresh IntegrationPlan) error {
	component := ""
	if len(approved.packageSources) != 0 {
		names := make([]string, 0, len(approved.packageSources)+len(fresh.packageSources))
		for name := range approved.packageSources {
			names = append(names, name)
		}
		for name := range fresh.packageSources {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if approved.packageSources[name] != fresh.packageSources[name] {
				component = name
				break
			}
		}
	}
	if component == "" {
		before := make(map[string]integrationFact, len(approved.facts))
		for _, fact := range approved.facts {
			before[fact.Key] = fact
		}
		for _, fact := range fresh.facts {
			old, exists := before[fact.Key]
			delete(before, fact.Key)
			if strings.HasPrefix(fact.Key, "lifecycle:") && (!exists || old.Desired != fact.Desired || !fact.AtDesired && (old.AtDesired || old.Observed != fact.Observed)) {
				component = strings.TrimPrefix(fact.Key, "lifecycle:")
				break
			}
		}
		if component == "" {
			for _, fact := range approved.facts {
				if _, remains := before[fact.Key]; remains && fact.Desired != "absent" && strings.HasPrefix(fact.Key, "lifecycle:") {
					component = strings.TrimPrefix(fact.Key, "lifecycle:")
					break
				}
			}
		}
	}
	return runtime.integrationDiagnostic(cause, profile.IntegrationDefinition{Name: component}, "apply", "observation-changed")
}
