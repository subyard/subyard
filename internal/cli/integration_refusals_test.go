package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

type independentOwnershipTestError string

func (err independentOwnershipTestError) Error() string { return string(err) }
func (err independentOwnershipTestError) ActivationDiagnostic() (string, string) {
	return "ownership conflict at " + string(err), "inspect " + string(err)
}

func TestIntegrationRefusalsRemainIndependentAcrossReleaseBoundary(t *testing.T) {
	first, second := independentOwnershipTestError("/home/dev/first"), independentOwnershipTestError("/home/dev/second")
	refusal := materializedConfigActivationError{
		cause: domain.CollectRefusals(second, first), yard: "default", phase: "integration plan", command: "integration status", action: "inspect",
	}.collected()
	var aggregate *domain.Refusals
	if !errors.As(refusal, &aggregate) || len(aggregate.Unwrap()) != 2 {
		t.Fatalf("refusals collapsed: %v", refusal)
	}
	for index, cause := range aggregate.Unwrap() {
		var diagnostic interface{ ActivationDiagnostic() (string, string) }
		if !errors.As(cause, &diagnostic) {
			t.Fatal("public diagnostic lost")
		}
		message, retry := diagnostic.ActivationDiagnostic()
		want := []string{string(first), string(second)}[index]
		if !strings.Contains(message, want) || !strings.Contains(retry, want) {
			t.Fatalf("finding lost: %s / %s", message, retry)
		}
	}
}
