package reconcileruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestIntegrationServiceConflictDiagnostics(t *testing.T) {
	incus := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{
		ExitCode: 1,
		Stdout:   []byte("synthetic-private-stdout"),
		Stderr:   []byte("synthetic-private-stderr\nsubyard-integration-conflict:ccusage\n"),
	}, Err: errors.New("guest command exited with status 1: synthetic-private-cause")}}}
	runtime := Runtime{Executor: incus, Yard: domain.Context{YardName: "example"}}
	fingerprint, changed, err := runtime.integrationServices(context.Background(), "observe")
	if err == nil || changed || fingerprint != "" {
		t.Fatalf("conflict did not fail closed: fingerprint=%q changed=%v error=%v", fingerprint, changed, err)
	}
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if !errors.As(err, &diagnostic) {
		t.Fatalf("known native conflict lost its public diagnostic: %v", err)
	}
	message, retry := diagnostic.ActivationDiagnostic()
	if want := "run " + shellquote.Command([]string{"yard", "-Y", "example", "integration", "status"}); retry != want {
		t.Fatalf("unsafe conflict retry: %q", retry)
	}
	for _, want := range []string{"ALLOWS_CODING_TOOLS=false", "/usr/local/bin/ccusage", "/var/lib/subyard/ccusage-ownership", "utility preserved"} {
		if !strings.Contains(message, want) {
			t.Fatalf("diagnostic omitted %q: %s", want, message)
		}
	}
	if len(message) > 512 || strings.ContainsAny(message+retry, "\r\n") {
		t.Fatalf("release diagnostic is not bounded/control-free: %q / %q", message, retry)
	}
	if message = err.Error(); strings.Contains(message, "synthetic-private") || strings.Contains(message, "--yes") {
		t.Fatalf("diagnostic exposed raw output or bypassed confirmation: %s", message)
	}
}

func TestIntegrationServiceUnknownErrorsStaySanitized(t *testing.T) {
	for _, stderr := range []string{
		"synthetic-private-stderr",
		"subyard-integration-conflict:unknown",
		"prefix subyard-integration-conflict:ccusage",
		"subyard-integration-conflict:ccusage synthetic-private-stderr",
	} {
		incus := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{
			Result: ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(stderr)},
			Err:    errors.New("synthetic-private-transport-error"),
		}}}
		runtime := Runtime{Executor: incus}
		_, _, err := runtime.integrationServices(context.Background(), "observe")
		if err == nil || err.Error() != "integration service or utility ownership is unknown or changed" {
			t.Fatalf("unknown failure was not sanitized: %v", err)
		}
		var diagnostic interface{ ActivationDiagnostic() (string, string) }
		if errors.As(err, &diagnostic) {
			t.Fatal("unknown native output was promoted to a public diagnostic")
		}
	}
}

func TestLifecycleNoticeIsBoundedAndOnlyForRetiredPackages(t *testing.T) {
	retired := profile.IntegrationDefinition{Name: "widget", Retired: true}
	active := profile.IntegrationDefinition{Name: "widget"}
	digest := strings.Repeat("a", 64)
	stale := `{"state":"stale","actual":"` + digest + `","desired":"` + strings.Repeat("b", 64) + `","notice":"remove widget"}`
	observation, err := decodeIntegrationLifecycleObservation(retired, []byte(stale))
	if err != nil || observation.Notice != "remove widget" || observation.State != ports.RuntimeStateStale || observation.Actual != digest {
		t.Fatalf("retirement notice lost: %+v, %v", observation, err)
	}
	if observation, err = decodeIntegrationLifecycleObservation(retired, []byte(`{"state":"absent","actual":"","desired":"","notice":"left in place"}`)); err != nil || observation.Notice != "left in place" {
		t.Fatalf("retained-state notice lost: %+v, %v", observation, err)
	}
	if observation, err = decodeIntegrationLifecycleObservation(active, []byte(`{"state":"absent","actual":"","desired":""}`)); err != nil || observation.Notice != "" {
		t.Fatalf("plain observation changed: %+v, %v", observation, err)
	}
	for name, test := range map[string]struct {
		definition profile.IntegrationDefinition
		payload    string
	}{
		"active package": {active, stale},
		"too long":       {retired, `{"state":"absent","actual":"","desired":"","notice":"` + strings.Repeat("x", integrationNoticeLimit+1) + `"}`},
		"control":        {retired, `{"state":"absent","actual":"","desired":"","notice":"a\nb"}`},
		"untrimmed":      {retired, `{"state":"absent","actual":"","desired":"","notice":" a"}`},
		"current state":  {retired, `{"state":"current","actual":"` + digest + `","desired":"` + digest + `","notice":"a"}`},
		"unknown field":  {retired, `{"state":"absent","actual":"","desired":"","extra":"a"}`},
		"trailing data":  {retired, `{"state":"absent","actual":"","desired":""} {}`},
		"invalid digest": {retired, `{"state":"absent","actual":"x","desired":"","notice":"a"}`},
	} {
		if _, err := decodeIntegrationLifecycleObservation(test.definition, []byte(test.payload)); err == nil {
			t.Errorf("%s: accepted %s", name, test.payload)
		}
	}
}
