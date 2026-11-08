package reconcileruntime

import (
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestAdoptionPrivilegeDiagnosticsReportAllSafeDifferences(t *testing.T) {
	runtime := Runtime{Yard: domain.Context{YardName: "default", YardKind: domain.YardContainer, NestedE2EVMs: true}}
	state := ports.ReconcileState{Instance: ports.InstanceInfo{Config: map[string]string{"security.syscalls.intercept.bpf": "false", "security.syscalls.intercept.bpf.devices": "unsafe private value"}, LocalConfig: map[string]string{"security.syscalls.intercept.bpf": "false", "security.syscalls.intercept.bpf.devices": "false"}}}
	err := runtime.adoptionKindPolicy(state, map[string]string{})
	if err == nil {
		t.Fatal("expected refusal")
	}
	message := err.Error()
	for _, expected := range []string{"security.syscalls.intercept.bpf  false -> true", "security.syscalls.intercept.bpf.local  false -> true", "security.syscalls.intercept.bpf.devices  differs -> true", "security.syscalls.intercept.bpf.devices.local  false -> true"} {
		if !strings.Contains(message, expected) {
			t.Errorf("missing %q in %s", expected, message)
		}
	}
	if strings.Contains(message, "unsafe private value") {
		t.Fatal("unvalidated native value leaked")
	}
}
