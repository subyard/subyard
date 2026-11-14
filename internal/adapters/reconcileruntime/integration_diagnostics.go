package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

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

// nativeDiagnosticProtocol is one source-owned stderr record family of native
// hooks: "<name> PHASE REASON", with a closed reason vocabulary.
type nativeDiagnosticProtocol struct {
	name    string
	reasons map[string]string
	// effectOnly reasons are valid only for an apply or verify record.
	effectOnly map[string]bool
}

var lifecycleDiagnosticProtocol = nativeDiagnosticProtocol{name: "subyard-lifecycle-diagnostic-v1", reasons: lifecycleReasons}

// record validates one record line for an operation phase and returns its
// reported phase and reason.
func (protocol nativeDiagnosticProtocol) record(line []byte, phase string) (string, string, bool) {
	fields := strings.Split(strings.TrimPrefix(string(line), protocol.name+" "), " ")
	if !bytes.HasPrefix(line, []byte(protocol.name+" ")) || len(fields) != 2 ||
		fields[0] != "inspect" && fields[0] != "apply" && fields[0] != "verify" {
		return "", "", false
	}
	if phase != "apply" && fields[0] != "inspect" || phase == "apply" && fields[0] == "inspect" {
		return "", "", false
	}
	if _, ok := protocol.reasons[fields[1]]; !ok || protocol.effectOnly[fields[1]] && fields[0] == "inspect" {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// failure selects the reason of exactly one well-formed record. Native output
// is private; duplicate, malformed or contradictory records fall back safely,
// including historical handlers without this optional transport.
func (protocol nativeDiagnosticProtocol) failure(stderr []byte, phase string) (string, string) {
	reportedPhase, reason := phase, "operation-failed"
	count := 0
	for _, line := range bytes.Split(stderr, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte(protocol.name)) {
			continue
		}
		count++
		recorded, recordedReason, ok := protocol.record(line, phase)
		if !ok {
			return phase, "operation-failed"
		}
		reason = recordedReason
		if phase == "apply" {
			reportedPhase = recorded
		}
	}
	if count != 1 {
		return phase, "operation-failed"
	}
	return reportedPhase, reason
}

func lifecycleFailure(stderr []byte, phase string) (string, string) {
	return lifecycleDiagnosticProtocol.failure(stderr, phase)
}

// Failure details use a closed generic vocabulary. Never copy arbitrary handler
// text, numeric formatting, command arguments or native output into diagnostics.
func readinessFailure(stderr []byte, phase string) string {
	const prefix = "subyard-readiness-diagnostic-v1 "
	var detail []string
	lifecycleRecords, readinessRecords := 0, 0
	recordPhase := ""
	for _, line := range bytes.Split(stderr, []byte{'\n'}) {
		if bytes.HasPrefix(line, []byte(lifecycleDiagnosticProtocol.name)) {
			lifecycleRecords++
			reported, _, ok := lifecycleDiagnosticProtocol.record(line, phase)
			if !ok {
				return ""
			}
			recordPhase = reported
		}
		if bytes.HasPrefix(line, []byte("subyard-readiness-diagnostic-v1")) {
			readinessRecords++
			if !bytes.HasPrefix(line, []byte(prefix)) || len(line) > 160 {
				return ""
			}
			detail = strings.Split(strings.TrimPrefix(string(line), prefix), " ")
		}
	}
	if lifecycleRecords != 1 || readinessRecords != 1 || len(detail) != 5 {
		return ""
	}
	if detail[0] != recordPhase {
		return ""
	}
	if !slices.Contains([]string{"instance-query", "owner-address", "enumeration", "service-state", "service-window", "recovery-enumeration", "inspection", "ownership", "installation", "native-producer"}, detail[1]) {
		return ""
	}
	if !slices.Contains([]string{"transport-timeout", "os-error", "native-exit", "native-timeout", "invalid-schema", "dependency-missing", "native-rejected", "startup-deadline", "service-inactive", "service-failed", "service-deactivating", "service-unknown"}, detail[2]) {
		return ""
	}
	values := [2]int{}
	for index, value := range detail[3:] {
		number, err := strconv.Atoi(value)
		if err != nil || number < 0 || number > 1320000 || strconv.Itoa(number) != value {
			return ""
		}
		values[index] = number
	}
	return fmt.Sprintf(" (probe %s; %s; elapsed %d ms; budget %d ms)", detail[1], detail[2], values[0], values[1])
}

func (runtime Runtime) integrationExecutionDiagnostic(cause error, definition profile.IntegrationDefinition, phase, recordPhase, reason string, stderr []byte, elapsed, budget time.Duration, callerError error) error {
	detail := readinessFailure(stderr, recordPhase)
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		category := "handler-deadline"
		if errors.Is(callerError, context.Canceled) {
			category = "caller-cancelled"
		} else if errors.Is(callerError, context.DeadlineExceeded) {
			category = "caller-deadline"
		}
		detail = fmt.Sprintf(" (probe handler; %s; elapsed %d ms; budget %d ms)", category, min(max(elapsed.Milliseconds(), 0), budget.Milliseconds()), budget.Milliseconds())
	}
	diagnostic := runtime.integrationDiagnostic(cause, definition, phase, reason)
	if failure, ok := diagnostic.(*integrationLifecycleError); ok {
		failure.message += detail
	}
	return diagnostic
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
