package reconcileruntime

import (
	"bytes"
	"crypto/sha256"
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

type integrationFailure struct {
	domain.Finding
	cause                        error
	conflict, ownership, pending bool
}

func (failure integrationFailure) Error() string {
	if failure.ownership || failure.pending {
		return failure.Message + " Next, on the owner host: " + failure.Recovery
	}
	return failure.Message
}

func (failure integrationFailure) Unwrap() error                     { return failure.cause }
func (failure integrationFailure) ActivationFinding() domain.Finding { return failure.Finding }
func (failure integrationFailure) ActivationDiagnostic() (string, string) {
	return failure.Message, failure.Recovery
}
func (failure integrationFailure) ActivationConflict() bool              { return failure.conflict }
func (failure integrationFailure) integrationOwnershipConflict() bool    { return failure.ownership }
func (failure integrationFailure) PendingIntegrationConfiguration() bool { return failure.pending }

func integrationOwnershipFailure(message, recovery string, pending bool) integrationFailure {
	return integrationFailure{Finding: domain.Finding{Message: message, Recovery: recovery}, ownership: true, pending: pending}
}

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
	if item.Owner != "" {
		message = item.label()
		retry += " " + item.Owner
	}
	if item.yard != "" {
		message += " in yard " + item.yard
	}
	if reason == "observation-changed" && !errors.Is(cause, domain.ErrPlanStale) {
		cause = fmt.Errorf("%w: %w", domain.ErrPlanStale, cause)
	}
	item.Message, item.Recovery = message+": "+lifecycleReasons[reason], retry+" --json"
	conflict := reason == "ambiguous-ownership" || reason == "foreign-ownership"
	if !conflict {
		item.ReleaseRecovery = "run yard migrate"
	}
	if _, ok := domain.PublicFinding(cause); ok {
		return item.refusal(cause)
	}
	return integrationFailure{Finding: item.Finding, cause: cause, conflict: conflict}
}

// Bind the input before reading native state. The same finding identifies a
// failed read or a successful read that proves the input is still drifting.
type integrationWorkItem struct {
	domain.Finding
	yard, step, location string
}

func (runtime Runtime) integrationItem(step, owner string) integrationWorkItem {
	yard := runtime.Yard.YardName
	if len(yard) > 63 || !domain.SafeName(yard) {
		yard = ""
	}
	retry := "run yard init"
	if runtime.PersistFinishOnly {
		retry = "run yard migrate"
	}
	return (integrationWorkItem{Finding: domain.Finding{Input: step, Recovery: retry}, yard: yard, step: step}).withOwner(owner)
}

func (item integrationWorkItem) withOwner(owner string) integrationWorkItem {
	item.Owner = ""
	if len(owner) <= 63 && domain.SafeName(owner) {
		item.Owner = owner
	}
	return item
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
	if filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsFunc(path, unicode.IsControl) {
		return item.forSource(path)
	}
	return item
}

func (item integrationWorkItem) forSource(path string) integrationWorkItem {
	location := fmt.Sprintf("%q", path)
	item.Input += " at " + location
	if len(location) <= 256 {
		item.location = location
	}
	return item
}

func (item integrationWorkItem) forReportedArtifact(entry integrationArtifact) integrationWorkItem {
	if entry.declaredPath != "" {
		return item.atDestination(entry.declaredPath)
	}
	// Unmatched evidence can distinguish failures without being displayed.
	item.Input += fmt.Sprintf(" %x", sha256.Sum256([]byte(entry.Path)))
	return item
}

func (item integrationWorkItem) label() string {
	if item.Owner == "" {
		return "integration " + item.step
	}
	return "integration " + item.Owner + " " + item.step
}

func (item integrationWorkItem) refusal(cause error) error {
	if cause == nil || item.Input == "" {
		return cause
	}
	if leaves, aggregate := domain.IndependentRefusals(cause); aggregate {
		var public []error
		for index, leaf := range leaves {
			part := item
			part.Input += fmt.Sprintf(" [%d]", index+1)
			public = append(public, part.refusal(leaf))
		}
		return domain.CollectRefusals(public...)
	}
	if existing, ok := domain.PublicFinding(cause); ok {
		if existing.Input != "" {
			return cause
		}
		existing.Owner, existing.Input = item.Owner, item.Input
		if item.Owner != "" {
			existing.Message = item.label() + ": " + existing.Message
		}
		var pending interface{ PendingIntegrationConfiguration() bool }
		var conflict interface{ ActivationConflict() bool }
		return integrationFailure{Finding: existing, cause: cause, ownership: IntegrationOwnershipConflict(cause),
			pending:  errors.As(cause, &pending) && pending.PendingIntegrationConfiguration(),
			conflict: errors.As(cause, &conflict) && conflict.ActivationConflict()}
	}
	return item.failure(cause, "")
}

func (item integrationWorkItem) failure(cause error, detail string) error {
	item.Message = item.label() + " failed"
	if item.yard != "" {
		item.Message += " in yard " + item.yard
	}
	if item.location != "" {
		item.Message += " at " + item.location
	}
	item.Message += detail
	return integrationFailure{Finding: item.Finding, cause: cause}
}

// Inventory fact keys identify artifacts, but only the matched declarations
// supply public names and paths. Deferred paths had no independent observation.
func (runtime Runtime) bindInventoryFacts(facts []integrationFact, desired, retired []integrationArtifact, projection map[string]string, item integrationWorkItem) {
	items := make(map[string]integrationWorkItem, len(desired)+len(retired))
	bind := func(entry integrationArtifact, work integrationWorkItem) {
		if projection[entry.Path] == "deferred" {
			return
		}
		key := entry.Path
		if key == "" {
			key = entry.ID
		}
		items[entry.Kind+":"+key] = work
	}
	for _, entry := range retired {
		if entry.declaredOwner != "" {
			bind(entry, item.withOwner(entry.declaredOwner).forReportedArtifact(entry))
		}
	}
	for _, entry := range desired {
		bind(entry, item.withOwner(entry.ID).atDestination(entry.Path))
	}
	for i := range facts {
		facts[i].item = items[facts[i].Key]
	}
}

func (runtime Runtime) integrationVerificationRefusals(plan IntegrationPlan, readErr, driftCause error) error {
	findings := []error{readErr}
	for _, fact := range plan.facts {
		if !fact.AtDesired && fact.item.Input != "" {
			findings = append(findings, fact.item.failure(driftCause, ": "+lifecycleReasons["convergence-failed"]))
		}
	}
	if plan.Changed && len(findings) == 1 {
		findings = append(findings, runtime.integrationDiagnostic(driftCause, profile.IntegrationDefinition{}, "verify", "convergence-failed"))
	}
	return collectIntegrationRefusals(findings...)
}

func (runtime Runtime) integrationApplyDiagnostic(cause error, step string) error {
	return runtime.integrationItem("apply "+step, "").refusal(cause)
}

// The integration reads report independent leaves across substeps.
func collectIntegrationRefusals(causes ...error) error {
	var findings []error
	for _, cause := range causes {
		findings = append(findings, domain.RefusalCauses(cause)...)
	}
	return domain.CollectRefusals(findings...)
}

// IntegrationPlanDiagnostic attributes a stale guard to the inputs that changed.
func (runtime Runtime) IntegrationPlanDiagnostic(cause error, approved, fresh IntegrationPlan, exact bool) error {
	var findings []error
	for name, binding := range approved.packageSources {
		if fresh.packageSources[name] != binding {
			findings = append(findings, runtime.integrationDiagnostic(cause, profile.IntegrationDefinition{Name: name}, "apply", "observation-changed"))
		}
	}
	for name := range fresh.packageSources {
		if _, existed := approved.packageSources[name]; !existed {
			findings = append(findings, runtime.integrationDiagnostic(cause, profile.IntegrationDefinition{Name: name}, "apply", "observation-changed"))
		}
	}
	changed, missing := compareIntegrationFacts(approved, fresh, exact)
	for _, fact := range slices.Concat(changed, missing) {
		if fact.item.Input != "" {
			findings = append(findings, fact.item.failure(cause, ": "+lifecycleReasons["observation-changed"]))
		}
	}
	if result := domain.CollectRefusals(findings...); result != nil {
		return result
	}
	return cause
}
