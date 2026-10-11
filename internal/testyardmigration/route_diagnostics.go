package testyardmigration

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

type routeActivationDiagnostic struct {
	cause          error
	message, retry string
	conflict       bool
}

func (failure *routeActivationDiagnostic) Error() string            { return failure.cause.Error() }
func (failure *routeActivationDiagnostic) Unwrap() error            { return failure.cause }
func (failure *routeActivationDiagnostic) ActivationConflict() bool { return failure.conflict }
func (failure *routeActivationDiagnostic) ActivationDiagnostic() (string, string) {
	return failure.message, failure.retry
}

// Only fixed reasons and validated native identities cross the release boundary.
// Raw configuration, device properties and transport errors remain in cause.
func routeDiagnostic(cause error, reason, yard, project, instance string, conflict bool) error {
	if cause == nil {
		return nil
	}
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if errors.As(cause, &diagnostic) {
		return cause
	}
	message := "route consumer activation"
	retry := "incus list --all-projects --format=json"
	if len(yard) <= 63 && domain.SafeName(yard) {
		message += " for yard " + yard
	}
	if len(project) <= 96 && len(instance) <= 96 && domain.SafeID(project) && domain.SafeID(instance) {
		message += fmt.Sprintf(" (%s/%s)", project, instance)
		retry = fmt.Sprintf("incus config show %s --project %s --expanded", instance, project)
	}
	return &routeActivationDiagnostic{cause: cause, message: message + ": " + reason, retry: retry, conflict: conflict}
}

func routeConfigDiagnostic(cause error, reason, yard, configHome string) error {
	if cause == nil {
		return nil
	}
	var existing interface{ ActivationDiagnostic() (string, string) }
	if errors.As(cause, &existing) {
		return cause
	}
	err := routeDiagnostic(cause, reason, yard, "", "", false)
	var diagnostic *routeActivationDiagnostic
	if errors.As(err, &diagnostic) && len(yard) <= 63 && domain.SafeName(yard) {
		diagnostic.retry = "run yard -Y " + yard + " config validate"
		if retry := config.SourceAliasRepairRetry(configHome, yard, cause); retry != "" {
			diagnostic.retry = retry
		}
		if source, ok := config.SourceDiagnostic(configHome, cause); ok {
			available := 512 - len(diagnostic.message) - 2
			if len(source) > available {
				source = source[:available-3]
				for !utf8.ValidString(source) {
					source = source[:len(source)-1]
				}
				source += "..."
			}
			diagnostic.message += ": " + source
		}
	}
	return err
}

func ownerProjectDiagnostic(cause error, reason string, conflict bool) error {
	return &routeActivationDiagnostic{
		cause: cause, message: "route consumer activation for yard test-yard: " + reason,
		retry: "incus project list --format=json", conflict: conflict,
	}
}

func ownerStateDiagnostic(cause error, reason string) error {
	return &routeActivationDiagnostic{
		cause: cause, message: "route consumer activation for yard test-yard: " + reason,
		retry:    "inspect and repair the conflicting owner registration or controller state, then run yard update",
		conflict: true,
	}
}
