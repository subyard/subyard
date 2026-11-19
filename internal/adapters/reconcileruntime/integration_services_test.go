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
		Stderr:   []byte("synthetic-private-stderr\nsubyard-integration-conflict:ccusage\n" + bubblewrapRefusalPrefix + "the owned policy directory /opt/subyard/bwrap-apparmor is missing or changed" + bubblewrapRefusalSuffix + "\n"),
	}, Err: errors.New("guest command exited with status 1: synthetic-private-cause")}}}
	runtime := Runtime{Executor: incus, Yard: domain.Context{YardName: "example"}}
	fingerprint, changed, err := runtime.integrationServices(context.Background(), "observe")
	if err == nil || changed || fingerprint != "" {
		t.Fatalf("conflict did not fail closed: fingerprint=%q changed=%v error=%v", fingerprint, changed, err)
	}
	var diagnostic integrationOwnershipError
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
	if !strings.Contains(message, "Bubblewrap AppArmor: the owned policy directory /opt/subyard/bwrap-apparmor is missing or changed. Inspect") {
		t.Fatalf("independent Bubblewrap refusal was lost: %s", message)
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

func TestBubblewrapPolicyRefusalDiagnosticIsBounded(t *testing.T) {
	retry := "run 'yard' '-Y' 'example' 'integration' 'status'"
	for _, reason := range bubblewrapRefusalReasons() {
		line := bubblewrapRefusalPrefix + reason + bubblewrapRefusalSuffix
		message, got := bubblewrapPolicyRefusal{line: line, yard: "example", retry: retry}.ActivationDiagnostic()
		if got != retry || message != "yard example: "+line {
			t.Fatalf("unexpected diagnostic for %q: %q / %q", line, message, got)
		}
	}
	if message, _ := (bubblewrapPolicyRefusal{line: bubblewrapRefusalPrefix + "x", yard: strings.Repeat("y", 64), retry: retry}).ActivationDiagnostic(); message != "" {
		t.Fatalf("invalid yard name was rendered: %q", message)
	}
}

// The policy program prints every refusal with the suffix the host matches.
func TestBubblewrapRefusalTextMatchesThePolicyProgram(t *testing.T) {
	if !strings.Contains(bubblewrapPolicy, "printf '"+bubblewrapRefusalPrefix+"%s"+bubblewrapRefusalSuffix+"\\n'") {
		t.Fatal("policy program refusal format changed")
	}
	for line, want := range map[string]string{
		bubblewrapRefusalPrefix + "the yard cannot load AppArmor profiles" + bubblewrapRefusalSuffix:                "the yard cannot load AppArmor profiles",
		bubblewrapRefusalPrefix + "administrator systemd override at /run/" + guestCanary + bubblewrapRefusalSuffix: "administrator systemd override in a systemd unit path",
		bubblewrapRefusalPrefix + "administrator AppArmor policy at /etc/" + guestCanary + bubblewrapRefusalSuffix:  "the yard policy program refused with an unrecognized reason",
		bubblewrapRefusalPrefix + guestCanary: "the yard policy program refused with an unrecognized reason",
	} {
		if got := bubblewrapRefusalLine(guestLines([]byte(line))[0]); got != bubblewrapRefusalPrefix+want+bubblewrapRefusalSuffix {
			t.Fatalf("%q rendered as %q", line, got)
		}
	}
}
