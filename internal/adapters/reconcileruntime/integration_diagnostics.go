package reconcileruntime

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

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
// hooks: "<name> [NONCE] PHASE REASON", with a closed reason vocabulary.
type nativeDiagnosticProtocol struct {
	name    string
	reasons map[string]string
	// effectOnly reasons are valid only for an apply or verify record.
	effectOnly map[string]bool
	// nonce, when set, must open the record. Only the handler process receives
	// it, so output that the handler relays from elsewhere cannot forge one.
	nonce string
}

// record validates one record line for an operation phase and returns its
// reported phase and reason.
func (protocol nativeDiagnosticProtocol) record(line []byte, phase string) (string, string, bool) {
	fields := strings.Split(strings.TrimPrefix(string(line), protocol.name+" "), " ")
	if protocol.nonce != "" {
		if len(fields) == 0 || fields[0] != protocol.nonce {
			return "", "", false
		}
		fields = fields[1:]
	}
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

func (runtime Runtime) integrationDiagnostic(cause error, definition profile.IntegrationDefinition, phase, reason string) error {
	if cause == nil && reason != "convergence-failed" {
		return nil
	}
	if _, ok := lifecycleReasons[reason]; !ok {
		reason = "operation-failed"
	}
	if phase != "inspect" && phase != "apply" && phase != "verify" {
		phase = "inspect"
	}
	item := runtime.integrationItem(phase, definition.Name)
	message, command := "integration lifecycle "+phase, "yard"
	if item.yard != "" {
		command += " -Y " + item.yard
	}
	retry := command + " integration status"
	if item.owner != "" {
		message = item.label()
		retry += " " + item.owner
	}
	if item.yard != "" {
		message += " in yard " + item.yard
	}
	if reason == "observation-changed" && !errors.Is(cause, domain.ErrPlanStale) {
		cause = fmt.Errorf("%w: %w", domain.ErrPlanStale, cause)
	}
	return item.diagnostic(cause, &integrationLifecycleError{cause: cause, message: message + ": " + lifecycleReasons[reason], retry: retry + " --json", conflict: reason == "ambiguous-ownership" || reason == "foreign-ownership"})
}

// integrationWorkItem binds the declaring owner and step before a native read
// or effect. The same item describes a refusal and successfully observed drift.
type integrationWorkItem struct {
	step, owner, yard, retry, destination string
}

func (runtime Runtime) integrationItem(step, owner string) integrationWorkItem {
	if len(owner) > 63 || !domain.SafeName(owner) {
		owner = ""
	}
	yard := runtime.Yard.YardName
	if len(yard) > 63 || !domain.SafeName(yard) {
		yard = ""
	}
	retry := "run yard init"
	if runtime.PersistFinishOnly {
		retry = "run yard migrate"
	}
	return integrationWorkItem{step: step, owner: owner, yard: yard, retry: retry}
}

func (runtime Runtime) integrationRead(phase, step, owner string) integrationWorkItem {
	switch phase {
	case "apply":
		phase = "apply observe"
	case "verify":
		phase = "apply verify"
	default:
		phase = "inspect"
	}
	if step != "" {
		phase += " " + step
	}
	return runtime.integrationItem(phase, owner)
}

// Only paths from the declaring work item may be rendered.
func (item integrationWorkItem) atDestination(path string) integrationWorkItem {
	if len(path) <= 256 && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsFunc(path, unicode.IsControl) {
		item.destination = path
	}
	return item
}

func (item integrationWorkItem) label() string {
	if item.owner == "" {
		return "integration " + item.step
	}
	return "integration " + item.owner + " " + item.step
}

type scopedIntegrationDiagnostic struct {
	cause          error
	message, retry string
}

func (failure *scopedIntegrationDiagnostic) Error() string {
	return failure.message + " Next, on the owner host: " + failure.retry
}
func (failure *scopedIntegrationDiagnostic) Unwrap() error { return failure.cause }
func (failure *scopedIntegrationDiagnostic) ActivationDiagnostic() (string, string) {
	return failure.message, failure.retry
}

func (item integrationWorkItem) diagnostic(cause, fallback error) error {
	var existing interface{ ActivationDiagnostic() (string, string) }
	if !errors.As(cause, &existing) {
		return fallback
	}
	if item.owner == "" {
		return cause
	}
	// An enclosing batch must preserve a diagnostic already bound at dispatch.
	var scoped *scopedIntegrationDiagnostic
	if errors.As(cause, &scoped) {
		return cause
	}
	message, retry := existing.ActivationDiagnostic()
	return &scopedIntegrationDiagnostic{cause, item.label() + ": " + message, retry}
}

func (item integrationWorkItem) refusal(cause error) error {
	if cause == nil || item.step == "" {
		return cause
	}
	if refusals, ok := cause.(*domain.Refusals); ok {
		var public []error
		for _, finding := range refusals.Unwrap() {
			public = append(public, item.refusal(finding))
		}
		return domain.CollectRefusals(public...)
	}
	return item.diagnostic(cause, item.failure(cause, ""))
}

func (item integrationWorkItem) failure(cause error, detail string) error {
	message := item.label() + " failed"
	if item.yard != "" {
		message += " in yard " + item.yard
	}
	if item.destination != "" {
		message += fmt.Sprintf(" at %q", item.destination)
	}
	return &integrationLifecycleError{cause: cause, message: message + detail, retry: item.retry}
}

func (runtime Runtime) integrationVerificationRefusals(plan IntegrationPlan, readErr, driftCause error) error {
	findings := []error{readErr}
	for _, item := range plan.drift {
		findings = append(findings, item.failure(driftCause, ": "+lifecycleReasons["convergence-failed"]))
	}
	if plan.Changed && len(plan.drift) == 0 {
		findings = append(findings, runtime.integrationDiagnostic(driftCause, profile.IntegrationDefinition{}, "verify", "convergence-failed"))
	}
	return collectIntegrationRefusals(findings...)
}

func (runtime Runtime) integrationApplyDiagnostic(cause error, step string) error {
	return runtime.integrationItem("apply "+step, "").refusal(cause)
}

// Flatten read results after their refusal leaves have been scoped.
func collectIntegrationRefusals(causes ...error) error {
	var findings []error
	for _, cause := range causes {
		if aggregate, ok := cause.(*domain.Refusals); ok {
			findings = append(findings, aggregate.Unwrap()...)
		} else {
			findings = append(findings, cause)
		}
	}
	return domain.CollectRefusals(findings...)
}

func (runtime Runtime) knownIntegrationID(id string) string {
	if id == "" {
		return ""
	}
	definitions, err := runtime.integrationDefinitions()
	if err == nil && slices.ContainsFunc(definitions, func(definition profile.IntegrationDefinition) bool {
		return definition.Name == id
	}) {
		return id
	}
	return ""
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
	return runtime.integrationDiagnostic(cause, profile.IntegrationDefinition{Name: component}, "apply", "observation-changed")
}
