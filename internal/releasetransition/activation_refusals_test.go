package releasetransition

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

type pendingConfigurationFinding string

func (finding pendingConfigurationFinding) Error() string                        { return string(finding) }
func (finding pendingConfigurationFinding) ActivationPendingConfiguration() bool { return true }
func (finding pendingConfigurationFinding) ActivationDiagnostic() (string, string) {
	return string(finding), "inspect " + string(finding)
}

func TestActivationObservationKeepsEveryRefusalAndItsPendingClass(t *testing.T) {
	blocker := activationObservationBlocker(context.Background(), "fixture", domain.CollectRefusals(pendingConfigurationFinding("second"), pendingConfigurationFinding("first")))
	if !strings.Contains(blocker.Message, "first") || !strings.Contains(blocker.Message, "second") ||
		blocker.Resource != "activation.fixture.pending-configuration" {
		t.Fatalf("refusals or their pending class lost: %#v", blocker)
	}
	// One finding that is not pending configuration makes the whole observation a blocker.
	blocker = activationObservationBlocker(context.Background(), "fixture", domain.CollectRefusals(pendingConfigurationFinding("first"), errors.New("untyped")))
	if blocker.Resource != "activation.fixture" {
		t.Fatalf("an untyped refusal was classified as pending configuration: %#v", blocker)
	}
}

type otherMigrationRefusal struct{ error }

func (otherMigrationRefusal) MigrationRefusal() bool { return true }

type nativeDiagnosticWrapper struct{ error }

func (wrapper nativeDiagnosticWrapper) Unwrap() error { return wrapper.error }

func (nativeDiagnosticWrapper) ActivationDiagnostic() (string, string) {
	return "wrapped diagnostic", "inspect wrapped diagnostic"
}
