package releaseowner

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Diagnostic carries only the owning component's explicitly public contract.
// It does not classify release outcomes or turn an unresolved effect into drain.
type Diagnostic struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Retry   string `json:"retry"`
}

func newComponentDiagnostic(kind, message, retry string) (*Diagnostic, error) {
	valid := func(text string) bool {
		return len(text) <= 512 && utf8.ValidString(text) && strings.IndexFunc(text, unicode.IsControl) < 0
	}
	if (kind != "component-failure" && kind != "component-conflict") || message == "" || !valid(message) || !valid(retry) {
		return nil, errors.New("invalid public component diagnostic")
	}
	return &Diagnostic{kind, message, retry}, nil
}

func componentDiagnostic(cause error) *Diagnostic {
	var public interface{ ActivationDiagnostic() (string, string) }
	if !errors.As(cause, &public) {
		return nil
	}
	message, retry := public.ActivationDiagnostic()
	kind := "component-failure"
	var conflict interface{ ActivationConflict() bool }
	if errors.As(cause, &conflict) && conflict.ActivationConflict() {
		kind = "component-conflict"
	}
	diagnostic, _ := newComponentDiagnostic(kind, message, retry)
	return diagnostic
}
