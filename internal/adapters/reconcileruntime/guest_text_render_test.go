package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

// canaryGuestExecutor reports the canary in every guest string field of the
// inventory, persist and structured configuration programs.
type canaryGuestExecutor struct {
	*applyDiagnosticExecutor
	conflict bool
}

func (fixture *canaryGuestExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	args := request.Command
	reply := func(value any) (ports.InstanceExecResult, error) {
		payload, _ := json.Marshal(value)
		return ports.InstanceExecResult{Stdout: payload}, nil
	}
	if len(args) < 5 || args[0] != "python3" {
		return fixture.applyDiagnosticExecutor.Exec(ctx, project, instance, request)
	}
	retired := []map[string]any{{"id": guestCanary, "kind": "structured", "format": "json", "path": "/home/dev/" + guestCanary},
		{"id": "sample", "kind": guestCanary, "path": "/home/dev/.memory/" + guestCanary}, {"id": guestCanary, "kind": "package"}}
	switch {
	case args[3] == IntegrationInventoryReader() && args[4] == "observe" && fixture.conflict:
		stderr, _ := json.Marshal(map[string]any{"retired": retired, "conflicts": []map[string]string{
			{"reason": guestCanary, "path": "/home/dev/.memory"},
			{"reason": "owned artifact drift", "path": "/home/dev/" + guestCanary},
			{"reason": "unowned selected artifact", "path": "/etc/subyard/agent-project-hooks", "detail": guestCanary},
			{"reason": "artifact evidence unavailable", "path": "/home/dev/.memory", "detail": guestCanary}}})
		return ports.InstanceExecResult{ExitCode: 1, Stderr: stderr}, &ports.GuestExitError{Code: 1}
	case args[3] == IntegrationInventoryReader() && args[4] == "observe":
		return reply(map[string]any{"fingerprint": strings.Repeat("a", 64), "changed": true, "retired": retired,
			"facts": []map[string]any{{"key": "file:/home/dev/" + guestCanary, "desired": "absent", "observed": "x"}}})
	case args[3] == integrationPersistProgram && args[4] == "observe":
		return reply(map[string]any{"fingerprint": strings.Repeat("b", 64), "entries": []map[string]any{{"integration": guestCanary,
			"source": "/home/dev/.memory", "target": "/" + guestCanary, "ops": []string{"finish", "adopt"}, "id": guestCanary, "state": "exchanged",
			"existing": []string{guestCanary}, "existing_count": 3}}})
	case args[3] == integrationPersistProgram && args[4] == "apply":
		return reply(map[string]any{"results": []map[string]any{{"source": "/home/dev/.memory", "not_transferred": []string{guestCanary},
			"retained": []string{"/home/dev/.memory.subyard-backup-" + guestCanary, "/" + guestCanary}}}})
	case args[3] == integrationPersistProgram && args[4] == "retained":
		return reply(map[string]any{"retained": []map[string]any{
			{"integration": guestCanary, "kind": "backup", "path": "/home/dev/.memory.subyard-backup-" + guestCanary, "bytes": 5},
			{"integration": "sample", "kind": "stage", "path": "/mnt/host/agent-sessions/sample/.subyard-stage-sample-" + guestCanary},
			{"integration": "sample", "kind": guestCanary, "path": "/" + guestCanary}}})
	case strings.Contains(args[3], "def retire("):
		return reply(map[string]any{"converged": false, "fingerprint": strings.Repeat("c", 64), "release": "changed",
			"released": []string{"/" + guestCanary}, "released_count": 1})
	}
	return fixture.applyDiagnosticExecutor.Exec(ctx, project, instance, request)
}

// renderedErrors collects the text and every public diagnostic of an error tree.
func renderedErrors(err error) []string {
	if err == nil {
		return nil
	}
	rendered := []string{err.Error(), fmt.Sprintf("%+v", err)}
	if public, ok := err.(interface{ ActivationDiagnostic() (string, string) }); ok {
		message, retry := public.ActivationDiagnostic()
		rendered = append(rendered, message, retry)
	}
	switch value := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range value.Unwrap() {
			rendered = append(rendered, renderedErrors(cause)...)
		}
	case interface{ Unwrap() error }:
		rendered = append(rendered, renderedErrors(value.Unwrap())...)
	}
	return rendered
}

func assertNoCanary(t *testing.T, where string, rendered ...string) {
	t.Helper()
	for _, text := range rendered {
		if strings.Contains(text, guestCanary) {
			t.Fatalf("%s rendered guest text: %s", where, text)
		}
	}
}

// No public output renders free guest text: plans, operation steps, apply
// output, refusals, their release diagnostics and retained listings.
func TestGuestTextNeverRendered(t *testing.T) {
	ctx := context.Background()
	for _, conflict := range []bool{false, true} {
		runtime, base, output := applyDiagnosticFixture(t, "persist")
		runtime.Stderr = output
		executor := &canaryGuestExecutor{applyDiagnosticExecutor: base, conflict: conflict}
		runtime.Executor = executor
		plan, err := runtime.IntegrationPlan(ctx)
		if conflict != (err != nil) {
			t.Fatalf("conflict=%t plan error: %v", conflict, err)
		}
		marshalled, _ := json.Marshal(plan)
		steps, _ := json.Marshal(plan.OperationSteps("subyard-sample/yard-sample"))
		facts, _ := json.Marshal(plan.facts)
		assertNoCanary(t, "plan", append(append(append(plan.Steps, plan.Warnings...), plan.Adoption...), plan.OwnershipReleases()...)...)
		assertNoCanary(t, "plan encoding", string(marshalled), string(steps), string(facts), fmt.Sprintf("%+v %#v", plan, plan))
		assertNoCanary(t, "plan refusal", renderedErrors(err)...)
		if !conflict {
			if !strings.Contains(strings.Join(plan.Steps, "\n"), "/home/dev/.memory") || !strings.Contains(strings.Join(plan.Steps, "\n"), "an undeclared path (sha256:") {
				t.Fatalf("plan lost declared or opaque locations: %q", plan.Steps)
			}
			executor.applyStarted = true
			assertNoCanary(t, "apply refusal", renderedErrors(runtime.ApplyIntegrations(ctx, plan))...)
			assertNoCanary(t, "apply output", output.String())
			if !strings.Contains(output.String(), "entries not transferred") || !strings.Contains(output.String(), "retained backup /home/dev/.memory.subyard-backup-*") {
				t.Fatalf("apply did not report the transfer outcome: %q", output.String())
			}
		}
		retained, err := runtime.ReadPersistRetained(ctx)
		encoded, _ := json.Marshal(retained)
		assertNoCanary(t, "retained", append(runtime.PersistRetainedLines(retained), string(encoded), fmt.Sprintf("%v", err))...)
		if len(retained) != 2 || retained[0].Integration != "sample" {
			t.Fatalf("retained items were not placed under declared names: %+v", retained)
		}
	}

	runtime := Runtime{Yard: domain.Context{YardName: "sample", YardInstanceName: "yard-sample", IncusProject: "subyard-sample"},
		Environment: []string{"CODING_TOOL_INTEGRATIONS=sample"}}
	mappings := []PersistMapping{{Integration: "sample", Source: "/home/dev/.memory", Target: "/mnt/host/agent-sessions/sample/memory", Selected: true}}
	var conflicts []map[string]string
	for _, reason := range []string{guestCanary, "unsafe persist data", "persist target entry refused", "persist mount boundary", "persist filesystem error",
		"persist paths overlap", "persist target filesystem", "persist space unavailable", "atomic exchange unsupported", "persist transfer identity unavailable"} {
		for _, path := range []string{"/home/dev/.memory", "/home/dev/.memory/" + guestCanary, "/" + guestCanary, guestCanary} {
			for _, detail := range []string{guestCanary, "Permission denied: " + guestCanary, "/mnt/host/" + guestCanary, "needs 1 bytes plus a 1 GiB reserve; " + guestCanary} {
				conflicts = append(conflicts, map[string]string{"reason": reason, "path": path, "detail": detail, "integration": guestCanary})
			}
		}
	}
	stderr, _ := json.Marshal(map[string]any{"conflicts": conflicts, "entries": []map[string]any{{"source": guestCanary, "persist": "deferred"}}})
	refusal := runtime.persistRefusal(stderr, mappings)
	assertNoCanary(t, "persist refusal", renderedErrors(refusal)...)
	if !strings.Contains(refusal.Error(), `at "/home/dev/.memory";`) {
		t.Fatalf("declared path was not shown: %v", refusal)
	}
	failure, _ := json.Marshal(map[string]string{"error": guestCanary})
	assertNoCanary(t, "persist failure", renderedErrors(runtime.persistRefusal(failure, mappings))...)

	services := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 1,
		Stderr: []byte(bubblewrapRefusalPrefix + guestCanary + "\n" + bubblewrapRefusalPrefix + "administrator systemd override at /" + guestCanary + bubblewrapRefusalSuffix + "\n")},
		Err: &ports.GuestExitError{Code: 1}}}}
	_, _, err := (Runtime{Executor: services, Yard: domain.Context{YardName: "sample"}}).integrationServices(context.Background(), "observe")
	assertNoCanary(t, "services refusal", renderedErrors(err)...)
	var buffer bytes.Buffer
	fmt.Fprint(&buffer, err)
	assertNoCanary(t, "services print", buffer.String())
}

// Every string a guest decode struct holds is guest text, except values that
// are only hashed, compared or validated against the catalog before use.
func TestGuestWireTypesHoldOnlyGuestText(t *testing.T) {
	allowed := map[string]bool{
		"persistObservationWire.Fingerprint": true, "integrationObservationWire.Fingerprint": true,
		"integrationFactWire.Desired": true, "integrationFactWire.Observed": true,
		"integrationObservationWire.Selected": true, "integrationObservationWire.AppliedSettings": true,
		"IntegrationDiagnosticEvidence.Key": true, "IntegrationDiagnosticEvidence.Digests": true, "IntegrationDiagnosticEvidence.Files": true,
		"IntegrationDiagnosticFile.Path": true, "IntegrationDiagnosticFile.Digest": true,
	}
	guest := reflect.TypeOf(guestText{})
	var walk func(reflect.Type, string)
	walk = func(kind reflect.Type, field string) {
		switch kind.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			if kind.Kind() == reflect.Map && kind.Key().Kind() == reflect.String && !allowed[field] {
				t.Errorf("%s has string keys", field)
			}
			walk(kind.Elem(), field)
		case reflect.String:
			if !allowed[field] {
				t.Errorf("%s holds a plain string", field)
			}
		case reflect.Struct:
			if kind == guest {
				return
			}
			for index := range kind.NumField() {
				walk(kind.Field(index).Type, kind.Name()+"."+kind.Field(index).Name)
			}
		}
	}
	for _, wire := range []any{persistObservationWire{}, persistOutcomeWire{}, persistRetainedWire{}, integrationObservationWire{}} {
		walk(reflect.TypeOf(wire), reflect.TypeOf(wire).Name())
	}
}
