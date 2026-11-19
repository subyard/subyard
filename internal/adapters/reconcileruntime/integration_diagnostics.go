package reconcileruntime

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
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
	component, yard := publicToken(definition.Name), publicToken(runtime.Yard.YardName)
	message := "integration lifecycle " + phase
	if component != "" {
		message = "integration " + component + " " + phase
	}
	if yard != "" {
		message += " in yard " + yard
	}
	retry := integrationStatusRetry(yard, component)
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
	return runtime.integrationDiagnostic(cause, profile.IntegrationDefinition{Name: component}, "apply", "observation-changed")
}

// publicToken returns a validated host name or ID, or nothing.
func publicToken(value string) string {
	if len(value) <= 63 && domain.SafeName(value) {
		return value
	}
	return ""
}

func integrationStatusRetry(yard, component string) string {
	retry := "yard"
	if yard != "" {
		retry += " -Y " + yard
	}
	retry += " integration status"
	if component != "" {
		retry += " " + component
	}
	return retry + " --json"
}

// integrationStep is the closed set of fail-fast integration apply steps.
type integrationStep string

const (
	stepObserve         integrationStep = "observe"
	stepHookScope       integrationStep = "hook scope"
	stepSettings        integrationStep = "settings capture"
	stepServices        integrationStep = "services"
	stepArtifacts       integrationStep = "artifacts"
	stepPersistApproval integrationStep = "persist approval"
	stepPersist         integrationStep = "persist"
	stepInventoryApply  integrationStep = "inventory update"
	stepConfigs         integrationStep = "configs"
	stepScript          integrationStep = "script"
	stepHookCapture     integrationStep = "hook capture"
	stepHooks           integrationStep = "hooks"
	stepRetire          integrationStep = "retire"
	stepInventoryCommit integrationStep = "inventory commit"
	stepVerify          integrationStep = "verify"
)

// stepFailure names a failed apply step for the release boundary. Its public
// diagnostic holds only host-owned tokens; the cause stays private.
type stepFailure struct {
	step        integrationStep
	owner, yard string
	cause       error
}

func (failure *stepFailure) message() string {
	message := "integration"
	if failure.owner != "" {
		message += " " + failure.owner
	}
	message += " apply " + string(failure.step) + " failed"
	if failure.yard != "" {
		message += " in yard " + failure.yard
	}
	return message
}

func (failure *stepFailure) Error() string { return failure.message() + ": " + failure.cause.Error() }
func (failure *stepFailure) Unwrap() error { return failure.cause }
func (failure *stepFailure) ActivationDiagnostic() (string, string) {
	return failure.message(), integrationStatusRetry(failure.yard, failure.owner)
}

// ownedError binds the declaring integration to a refusal at dispatch without
// changing its text, so a later step failure can name that owner.
type ownedError struct {
	owner string
	err   error
}

func (failure ownedError) Error() string { return failure.err.Error() }
func (failure ownedError) Unwrap() error { return failure.err }

func owned(owner string, err error) error {
	if err == nil {
		return nil
	}
	return ownedError{owner, err}
}

// stepFailure wraps each refusal of one step once. Existing public diagnostics
// pass through unchanged; an owner is shown only if the host declares it.
func (runtime Runtime) stepFailure(step integrationStep, owner string, cause error) error {
	if cause == nil {
		return nil
	}
	if refusals, ok := cause.(*domain.Refusals); ok {
		causes := []error{}
		for _, leaf := range refusals.Unwrap() {
			wrapped := runtime.stepFailure(step, owner, leaf)
			if nested, ok := wrapped.(*domain.Refusals); ok {
				causes = append(causes, nested.Unwrap()...)
			} else {
				causes = append(causes, wrapped)
			}
		}
		return domain.CollectRefusals(causes...)
	}
	// Source errors keep the boundary's own source rendering and repair retry.
	var public interface{ ActivationDiagnostic() (string, string) }
	var source *config.SourceError
	if errors.As(cause, &public) || errors.As(cause, &source) {
		return cause
	}
	var bound ownedError
	if owner == "" && errors.As(cause, &bound) {
		owner = bound.owner
	}
	return &stepFailure{step: step, owner: runtime.declaredOwner(owner), yard: publicToken(runtime.Yard.YardName), cause: cause}
}

// declaredOwner keeps an integration ID only if the host selection or catalog declares it.
func (runtime Runtime) declaredOwner(owner string) string {
	if publicToken(owner) == "" {
		return ""
	}
	if slices.Contains(strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")), owner) {
		return owner
	}
	definitions, err := runtime.integrationDefinitions()
	if err == nil && slices.ContainsFunc(definitions, func(definition profile.IntegrationDefinition) bool { return definition.Name == owner }) {
		return owner
	}
	return ""
}

// selectionOwner names the one selected integration whose AGENT_<id>_<setting> is value.
func (runtime Runtime) selectionOwner(setting, value string) string {
	owner := ""
	for _, agent := range strings.Fields(runtime.environmentValue("CODING_TOOL_INTEGRATIONS")) {
		if runtime.environmentValue("AGENT_"+agent+"_"+setting) == value {
			if owner != "" {
				return ""
			}
			owner = agent
		}
	}
	return owner
}

// driftOwner names the declared owner when every outstanding fact has that one owner.
func (runtime Runtime) driftOwner(plan IntegrationPlan) string {
	owner := ""
	for _, fact := range plan.facts {
		if fact.AtDesired {
			continue
		}
		if fact.owner == "" || owner != "" && fact.owner != owner {
			return ""
		}
		owner = fact.owner
	}
	return runtime.declaredOwner(owner)
}
