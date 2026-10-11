package paseo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	v1 "github.com/Subyard/Subyard/internal/releasetransition/protocol/v1"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestNativeOwnershipConflictPreservesServiceAndPublicCleanup(t *testing.T) {
	root := testkit.TempDir(t)
	unit := filepath.Join(root, "etc/systemd/system/paseo.service")
	for _, directory := range []string{filepath.Dir(unit), root + "/config", root + "/bin"} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const content = "[Service]\nExecStart=/synthetic-private-command\n"
	testkit.WriteFile(t, unit, []byte(content), 0600)
	testkit.WriteFile(t, root+"/config/projects-changed.sh", []byte("#!/bin/sh\n"), 0755)
	testkit.WriteFile(t, root+"/bin/systemctl", []byte("#!/bin/sh\nprintf unexpected >'"+root+"/service-call'\nexit 1\n"), 0755)
	engine := root + "/bin/yard-engine"
	testkit.WriteFile(t, engine, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0755)
	incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}}
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: incus, Executor: nativeProbe{root}, Integrations: []profile.IntegrationDefinition{}, Yard: domain.Context{YardName: "example"}, Environment: []string{"AGENT_paseo_CLEANUP=config/agents/paseo/cleanup.sh"}}
	for _, apply := range []bool{false, true} {
		var err error
		if apply {
			err = runtime.ApplyIntegrations(context.Background(), reconcileruntime.IntegrationPlan{})
		} else {
			_, err = runtime.IntegrationPlan(context.Background())
		}
		var diagnostic interface{ ActivationDiagnostic() (string, string) }
		if err == nil || !errors.As(err, &diagnostic) {
			t.Fatalf("native exit-1 conflict lost public diagnostic: %v", err)
		}
		publicMessage, retry := diagnostic.ActivationDiagnostic()
		message := err.Error()
		transition, transitionErr := releasetransition.NewV2Transition(releasetransition.V2Options{
			ConfigHome: testkit.TempDir(t), Releases: releasetransition.ReleasePair{From: "fixture", Target: "fixture"},
			ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
				return releasetransition.ReleaseLinks{Active: "fixture"}, nil
			},
			VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool { return false },
			ArtifactDigest:      releasetransition.Fingerprint(strings.Repeat("a", 64)),
			RegistryPayload:     []byte(`{"schemaVersion":2,"minimumEpochs":{"owner-registration":1,"power-metadata":1,"project-state":1,"settings":1},"currentEpochs":{"owner-registration":1,"power-metadata":1,"project-state":1,"settings":1},"migrations":[]}`),
			Reconcilers:         []releasetransition.V2ActivationReconciler{ownershipObservation{err}},
		})
		if transitionErr != nil {
			t.Fatal(transitionErr)
		}
		inspection, transitionErr := transition.Inspect(context.Background(), releasetransition.Goal{Target: "fixture", Direction: releasetransition.DirectionActivateTarget})
		if transitionErr != nil || inspection.Outcome == nil || inspection.Outcome.Message != publicMessage || inspection.Outcome.Retry != retry {
			t.Fatalf("actual V2 inspection discarded native diagnostic: %#v, %v", inspection, transitionErr)
		}
		if len(publicMessage) > 512 || strings.ContainsAny(publicMessage+retry, "\r\n") {
			t.Fatalf("unsafe release diagnostic: %q / %q", publicMessage, retry)
		}
		payload, transitionErr := json.Marshal(inspection)
		if transitionErr != nil {
			t.Fatal(transitionErr)
		}
		var wire v1.Inspection
		if transitionErr = json.Unmarshal(payload, &wire); transitionErr != nil {
			t.Fatal(transitionErr)
		}
		var encoded bytes.Buffer
		if transitionErr = v1.EncodeResponse(&encoded, v1.Response{SchemaVersion: 1, Inspection: &wire}); transitionErr != nil {
			t.Fatal(transitionErr)
		}
		if _, transitionErr = v1.DecodeResponse(&encoded); transitionErr != nil {
			t.Fatal(transitionErr)
		}

		command := []string{engine, "-Y", "example", "integration", "cleanup", "paseo"}
		if retry != shellquote.Command(append(append([]string(nil), command...), "--check")) {
			t.Fatalf("release retry is not read-only cleanup: %q", retry)
		}
		for _, argv := range [][]string{command, append(append([]string(nil), command...), "--check")} {
			line := shellquote.Command(argv)
			if !strings.Contains("\n"+message+"\n", "\n"+line+"\n") {
				t.Fatalf("missing cleanup command: %q", message)
			}
			output, err := exec.Command("sh", "-c", line).CombinedOutput()
			if err != nil || string(output) != strings.Join(argv[1:], "\n")+"\n" {
				t.Fatalf("cleanup argv was not parseable: %q, %v", output, err)
			}
		}
		if strings.Contains(message+retry, "synthetic-private") || strings.Contains(message+retry, "--yes") {
			t.Fatalf("unsafe diagnostic: %q / %q", message, retry)
		}
		data, err := os.ReadFile(unit)
		if err != nil || string(data) != content {
			t.Fatalf("unowned service changed: %v", err)
		}
	}
	runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=paseo"}
	if _, err := runtime.IntegrationPlan(context.Background()); err != nil {
		t.Fatalf("selected service rejected: %v", err)
	}
	if _, err := os.Stat(root + "/service-call"); !os.IsNotExist(err) {
		t.Fatal("unowned or selected service was touched")
	}
}

// Execute the actual native guard with guest paths mapped into a private fixture;
// other observation boundaries return synthetic converged results.
type nativeProbe struct{ root string }

func (fixture nativeProbe) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := append([]string(nil), request.Command...)
	if command[0] == "python3" {
		return ports.InstanceExecResult{Stdout: []byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","changed":false,"initial":false}`)}, nil
	}
	if len(command) < 4 || !strings.Contains(command[3], "subyard-integration-conflict:") {
		return ports.InstanceExecResult{}, nil
	}
	command[3] = strings.NewReplacer("/etc/", fixture.root+"/etc/", "/var/", fixture.root+"/var/", "/usr/", fixture.root+"/usr/").Replace(command[3])
	process := exec.CommandContext(ctx, command[0], command[1:]...)
	process.Env = append(os.Environ(), "PATH="+fixture.root+"/bin:"+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	err := process.Run()
	result := ports.InstanceExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		result.ExitCode = 1
	}
	return result, err
}

// The real transition projects the actual native observation failure.
type ownershipObservation struct{ cause error }

func (ownershipObservation) ID() string { return "fixture" }
func (fixture ownershipObservation) Observe(context.Context, releasetransition.ReleasePair, releasetransition.ReleaseLinks) (releasetransition.V2ActivationObservation, error) {
	return releasetransition.V2ActivationObservation{}, fixture.cause
}
func (ownershipObservation) Reconcile(context.Context, releasetransition.ReleaseLinks) error {
	return errors.New("observation must remain read-only")
}
