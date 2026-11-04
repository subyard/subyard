package testyardmigration

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerservice"
)

type routeActivationDiagnostic struct {
	cause          error
	message, retry string
	conflict       bool
	refusal        bool
}

func (failure *routeActivationDiagnostic) Error() string            { return failure.cause.Error() }
func (failure *routeActivationDiagnostic) Unwrap() error            { return failure.cause }
func (failure *routeActivationDiagnostic) ActivationConflict() bool { return failure.conflict }
func (failure *routeActivationDiagnostic) MigrationRefusal() bool {
	return failure.conflict || failure.refusal
}
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

func ownerProjectDiagnostic(options Options, cause error, reason string, conflict bool) error {
	retry := "incus project list --format=json"
	if conflict {
		return ownerStateDiagnostic(options, cause, reason)
	}
	return &routeActivationDiagnostic{
		cause: cause, message: "route consumer activation for yard test-yard: " + reason,
		retry: retry, conflict: conflict,
	}
}

func ownerStateDiagnostic(options Options, cause error, reason string) error {
	retry, err := RecreationHint(options, true)
	if err != nil {
		return err
	}
	return &routeActivationDiagnostic{cause: cause, message: "route consumer activation for yard test-yard: " + reason, retry: retry, conflict: true}
}

func reservedRegistrationDiagnostic(path, yard string) *routeActivationDiagnostic {
	relative := "yards/" + yard + "/config.env"
	if filepath.Base(path) == yard+".env" {
		relative = "yards/" + yard + ".env"
	}
	return &routeActivationDiagnostic{
		cause:   fmt.Errorf("%s does not select YARD_TEMPLATE=test-vms", yard),
		message: "reserved yard name " + yard + ": config-home-relative " + relative + " must select YARD_TEMPLATE=test-vms",
		retry:   "set YARD_TEMPLATE=test-vms in " + relative + ", or rename the unrelated yard registration, then run yard update --check",
		refusal: true,
	}
}

// Inspect the two reserved registrations independently; neither refusal hides
// the other. Inheritance remains supported only for the current registration.
func inspectReservedRegistration(options Options, path, yard string) error {
	payload, err := readRegistration(options, path)
	if err == nil && selectsTestVMs(string(payload)) {
		return nil
	}
	if err == nil && yard == CurrentYard && filepath.IsAbs(options.RepositoryRoot) {
		var loaded config.Loaded
		loaded, err = loadBrokerYard(options, CurrentYard)
		if err != nil {
			return routeConfigDiagnostic(err, "owner registration settings cannot be loaded", yard, options.ConfigHome)
		}
		if loaded.Environment["YARD_TEMPLATE"] == "test-vms" {
			return nil
		}
	}
	if err != nil {
		// Paths are derived from fixed names; never copy parser or filesystem text.
		diagnostic := reservedRegistrationDiagnostic(path, yard)
		diagnostic.cause = err
		diagnostic.message = strings.TrimSuffix(diagnostic.message, " must select YARD_TEMPLATE=test-vms") + " cannot be read safely"
		diagnostic.retry = "inspect and repair the named registration settings, then run yard update --check"
		diagnostic.refusal = false
		return diagnostic
	}
	return reservedRegistrationDiagnostic(path, yard)
}

func registrationRefusals(findings ...error) error {
	cause := domain.CollectRefusals(findings...)
	if _, public := cause.(interface{ ActivationDiagnostic() (string, string) }); public {
		return cause
	}
	var refusals *domain.Refusals
	if !errors.As(cause, &refusals) {
		return cause
	}
	var messages []string
	conflict, refusal := true, true
	actions := []string{}
	for _, finding := range refusals.Unwrap() {
		var diagnostic interface{ ActivationDiagnostic() (string, string) }
		if !errors.As(finding, &diagnostic) {
			return cause
		}
		message, action := diagnostic.ActivationDiagnostic()
		if !slices.Contains(actions, action) {
			actions = append(actions, action)
		}
		if len(message) > 254 {
			message = message[:251]
			for !utf8.ValidString(message) {
				message = message[:len(message)-1]
			}
			message += "..."
		}
		const ownerPrefix = "route consumer activation for yard test-yard"
		if len(messages) > 0 && strings.HasPrefix(messages[0], ownerPrefix) {
			message = strings.TrimSpace(strings.TrimPrefix(message, ownerPrefix))
			message = strings.TrimPrefix(message, ": ")
		}
		messages = append(messages, message)
		var ambiguous interface{ ActivationConflict() bool }
		conflict = conflict && errors.As(finding, &ambiguous) && ambiguous.ActivationConflict()
		var blocked interface{ MigrationRefusal() bool }
		refusal = refusal && errors.As(finding, &blocked) && blocked.MigrationRefusal()
	}
	retry := strings.Join(actions, ", then ")
	return &routeActivationDiagnostic{cause: cause, message: strings.Join(messages, "; "), conflict: conflict, refusal: refusal, retry: retry}
}

// RecreationHint names only executable commands for the actual service registration.
func RecreationHint(options Options, legacy bool) (string, error) {
	records, err := (ownerservice.Registry{Root: filepath.Join(options.DataHome, "services")}).List("test-vms")
	if err != nil {
		return "", err
	}
	next := "yard service remove test-vms, then yard service install test-vms"
	if len(records) == 0 {
		yard := CurrentYard
		if legacy {
			yard = LegacyYard
		}
		next = "yard -Y " + yard + " teardown --reset-config, then yard service install test-vms"
	} else if legacy {
		next = "yard -Y e2e-yard teardown --reset-config, then " + next
	}
	note := ""
	if err := config.CheckLocalSettingsWritable(options.ConfigHome); errors.Is(err, config.ErrLegacyGitSettingsMigration) {
		_, registered, err := configsync.ReadSourceRecord(options.ConfigHome)
		if err != nil {
			return "", err
		}
		if registered {
			next = "yard config sync, then " + next
			note = "; restore the registered configuration checkout first if unavailable"
		} else {
			next = "yard config sync <checkout>, then " + next
			note = "; replace <checkout> with your existing committed configuration checkout; restore it first if unavailable"
		}
	} else if err != nil {
		return "", err
	}
	return "test-yard requires recreation; preserve other sessions' VMs; run " + next + note, nil
}
