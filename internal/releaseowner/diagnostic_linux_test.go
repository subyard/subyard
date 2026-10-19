package releaseowner

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type publicComponentFailure struct{ message, retry string }

func (e publicComponentFailure) Error() string                          { return "private native output" }
func (e publicComponentFailure) ActivationDiagnostic() (string, string) { return e.message, e.retry }
func (e publicComponentFailure) ActivationConflict() bool               { return true }

func TestComponentDiagnosticPreservesOnlyBoundedTypedPublicCause(t *testing.T) {
	if componentDiagnostic(errors.New("private native output")) != nil {
		t.Fatal("raw error crossed component boundary")
	}
	public := publicComponentFailure{"source-owned conflict", "run yard migrate"}
	diagnostic := componentDiagnostic(fmt.Errorf("private wrapper: %w", public))
	if diagnostic == nil || *diagnostic != (Diagnostic{"component-conflict", public.message, public.retry}) {
		t.Fatal("typed original public cause lost", diagnostic)
	}
	for _, bad := range []string{"", strings.Repeat("x", 513), "control\n", "\xff"} {
		if componentDiagnostic(publicComponentFailure{bad, "safe retry"}) != nil {
			t.Fatal("invalid public diagnostic admitted")
		}
	}
	if _, err := newComponentDiagnostic("invented", "safe", ""); err == nil {
		t.Fatal("open diagnostic kind admitted")
	}
}
