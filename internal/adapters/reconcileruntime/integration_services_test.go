package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestIntegrationCleanupHintSurvivesCandidateDescriptorClose(t *testing.T) {
	root := filepath.Join(t.TempDir(), "candidate with 'quotes'")
	engine := filepath.Join(root, "bin", "yard-engine")
	if err := os.MkdirAll(filepath.Dir(engine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pin, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	runtime := Runtime{
		RepositoryRoot: fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), pin.Fd()),
		Environment:    []string{"AGENT_paseo_CLEANUP=config/agents/paseo/cleanup.sh"},
		Yard:           domain.Context{YardName: "example"},
	}
	hint := runtime.integrationCleanupHint("paseo")
	command := shellquote.Command([]string{engine, "-Y", "example", "integration", "cleanup", "paseo", "--check"})
	if !strings.Contains(hint, "\n"+command+"\n") || strings.Contains(hint, "/proc/") {
		t.Fatalf("candidate hint must name its persistent engine: %s", hint)
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil || string(output) != "-Y\nexample\nintegration\ncleanup\npaseo\n--check\n" {
		t.Fatalf("hint failed after updater exit: %s, %v", output, err)
	}
}

func TestIntegrationServiceConflictDiagnostics(t *testing.T) {
	for _, conflict := range []struct {
		id       string
		wantText []string
	}{
		{"paseo", []string{"integration paseo is not selected in CODING_TOOL_INTEGRATIONS", "/etc/systemd/system/paseo.service", "/var/lib/subyard/paseo-ownership", "service preserved", "releases before 0.15"}},
		{"aiobserver", []string{"integration aiobserver is not selected in CODING_TOOL_INTEGRATIONS", "/etc/subyard/ai-observer/managed", "/etc/systemd/system/subyard-ai-observer.service", "service preserved"}},
		{"ccusage", []string{"ALLOWS_CODING_TOOLS=false", "/usr/local/bin/ccusage", "/var/lib/subyard/ccusage-ownership", "utility preserved"}},
	} {
		for _, declared := range []bool{false, true} {
			name := conflict.id + "/no-cleanup"
			if declared {
				name = conflict.id + "/cleanup"
			}
			t.Run(name, func(t *testing.T) {
				incus := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{
					ExitCode: 1,
					Stdout:   []byte("synthetic-private-stdout"),
					Stderr:   []byte("synthetic-private-stderr\nsubyard-integration-conflict:" + conflict.id + "\n"),
				}}}}
				runtime := Runtime{Executor: incus, Yard: domain.Context{YardName: "example"}}
				if declared {
					runtime.Environment = []string{"AGENT_" + conflict.id + "_CLEANUP=config/agents/" + conflict.id + "/cleanup.sh"}
				}
				fingerprint, changed, err := runtime.integrationServices(context.Background(), "observe")
				if err == nil || changed || fingerprint != "" {
					t.Fatalf("conflict did not fail closed: fingerprint=%q changed=%v error=%v", fingerprint, changed, err)
				}
				message := err.Error()
				for _, want := range conflict.wantText {
					if !strings.Contains(message, want) {
						t.Fatalf("diagnostic omitted %q: %s", want, message)
					}
				}
				wantHint := declared && conflict.id != "ccusage"
				if gotHint := strings.Contains(message, "'integration' 'cleanup'"); gotHint != wantHint {
					t.Fatalf("cleanup hint=%v, want %v: %s", gotHint, wantHint, message)
				}
				if wantHint && !strings.Contains(message, "'yard' '-Y' 'example' 'integration' 'cleanup' '"+conflict.id+"' '--check'") {
					t.Fatalf("cleanup hint omitted the owner yard or read-only preview: %s", message)
				}
				if strings.Contains(message, "synthetic-private") || strings.Contains(message, "--yes") {
					t.Fatalf("diagnostic exposed raw output or bypassed confirmation: %s", message)
				}
			})
		}
	}
}

func TestIntegrationServiceUnknownErrorsStaySanitized(t *testing.T) {
	for _, stderr := range []string{
		"synthetic-private-stderr",
		"subyard-integration-conflict:unknown",
		"prefix subyard-integration-conflict:paseo",
		"subyard-integration-conflict:paseo synthetic-private-stderr",
	} {
		incus := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{
			Result: ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(stderr)},
			Err:    errors.New("synthetic-private-transport-error"),
		}}}
		runtime := Runtime{Executor: incus, Environment: []string{"AGENT_paseo_CLEANUP=config/agents/paseo/cleanup.sh"}}
		_, _, err := runtime.integrationServices(context.Background(), "observe")
		if err == nil || err.Error() != "integration service or utility ownership is unknown or changed" {
			t.Fatalf("unknown failure was not sanitized: %v", err)
		}
	}
}

// All guest paths are mapped into the fixture before executing the actual leaf.
type integrationServiceProbeExecutor struct{ root string }

func (fixture integrationServiceProbeExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	command := append([]string(nil), request.Command...)
	command[3] = strings.NewReplacer(
		"/etc/", fixture.root+"/etc/",
		"/var/", fixture.root+"/var/",
		"/usr/", fixture.root+"/usr/",
	).Replace(command[3])
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

func TestIntegrationServiceProbePreservesUnownedPaseoUnit(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "etc/systemd/system/paseo.service")
	for _, directory := range []string{filepath.Dir(unit), root + "/bin"} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const content = "[Service]\nExecStart=/synthetic-private-command\n"
	if err := os.WriteFile(unit, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/bin/systemctl", []byte("#!/bin/sh\nprintf unexpected >'"+root+"/service-call'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Executor: integrationServiceProbeExecutor{root}}
	for _, mode := range []string{"observe", "apply"} {
		_, _, err := runtime.integrationServices(context.Background(), mode)
		if err == nil || !strings.Contains(err.Error(), "integration paseo is not selected") {
			t.Fatalf("unowned unit was not diagnosed in %s: %v", mode, err)
		}
		data, readErr := os.ReadFile(unit)
		if readErr != nil || string(data) != content {
			t.Fatalf("unowned unit changed: %v", readErr)
		}
	}
	runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=paseo"}
	if _, changed, err := runtime.integrationServices(context.Background(), "observe"); err != nil || changed {
		t.Fatalf("selected integration was not skipped: changed=%v error=%v", changed, err)
	}
	if _, err := os.Stat(root + "/service-call"); !os.IsNotExist(err) {
		t.Fatal("unowned or selected integration touched a service")
	}
}
