package reconcileruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestIntegrationServiceConflictDiagnostics(t *testing.T) {
	incus := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{
		ExitCode: 1,
		Stdout:   []byte("synthetic-private-stdout"),
		Stderr:   []byte("synthetic-private-stderr\nsubyard-integration-conflict:ccusage\nBubblewrap AppArmor: synthetic-private-policy-path\nBubblewrap AppArmor: synthetic-private-unit-path\n"),
	}, Err: &ports.GuestExitError{Code: 1}}}}
	runtime := Runtime{Executor: incus, Yard: domain.Context{YardName: "example"}}
	fingerprint, changed, err := runtime.integrationServices(context.Background(), "observe")
	if err == nil || changed || fingerprint != "" {
		t.Fatalf("conflict did not fail closed: fingerprint=%q changed=%v error=%v", fingerprint, changed, err)
	}
	causes := domain.RefusalCauses(err)
	if len(causes) != 3 {
		t.Fatalf("expected three independent service refusals: %v", err)
	}
	usage, ok := domain.PublicFinding(causes[2])
	if !ok {
		t.Fatalf("known native conflict lost its public diagnostic: %v", err)
	}
	message, retry := usage.Message, usage.Recovery
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
	if !strings.Contains(message, bubblewrapPolicyRefusal) {
		t.Fatalf("independent Bubblewrap refusal was lost: %s", message)
	}
	for _, record := range []string{
		"subyard-integration-failure:bwrap-sandbox",
		"Bubblewrap AppArmor: the developer's Bubblewrap sandbox could not create user and network namespaces. Inspect the yard policy",
	} {
		runtime.Executor = &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(record)}}}}
		_, _, err := runtime.integrationServices(context.Background(), "observe")
		finding, ok := domain.PublicFinding(err)
		if !ok || !strings.Contains(finding.Message, "sandbox could not create") || IntegrationOwnershipConflict(err) {
			t.Fatalf("capability failure lost its classification: %v", err)
		}
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
