package statusruntime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

func TestIntegrationHealthIsBoundedAndDoesNotTrustInvalidOutput(t *testing.T) {
	for _, test := range []struct {
		name, output, want string
		exit               int
		err                error
	}{
		{name: "ready", output: `{"state":"ready"}`, want: "ready"},
		{name: "starting", output: `{"state":"starting"}`, want: "starting"},
		{name: "failed", output: `{"state":"failed"}`, want: "failed"},
		{name: "unknown", output: `{"state":"unknown"}`, want: "unknown"},
		{name: "unavailable", output: `{"state":"ready"}`, exit: 1, want: "unknown"},
		{name: "transport", output: `{"state":"ready"}`, err: errors.New("private transport detail"), want: "unknown"},
		{name: "empty", want: "unknown"},
		{name: "invalid state", output: `{"state":"healthy"}`, want: "unknown"},
		{name: "private detail", output: `{"state":"ready","detail":"private"}`, want: "unknown"},
		{name: "trailing report", output: `{"state":"ready"} {"state":"failed"}`, want: "unknown"},
		{name: "oversized", output: `{"state":"ready"}` + strings.Repeat(" ", 1024), want: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := Runtime{
				Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": "sample plain", "AGENT_sample_HEALTH": "sample-health"},
				Executor: statusExecutorFunc(func(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
					if _, ok := ctx.Deadline(); !ok || project != "project" || instance != "yard" || !reflect.DeepEqual(request.Command, []string{"sample-health"}) {
						t.Fatalf("unbounded or incorrect health probe: %#v", request)
					}
					return ports.InstanceExecResult{Stdout: []byte(test.output), ExitCode: test.exit}, test.err
				}),
			}
			result := runtime.ReadIntegrationHealth(context.Background(), domain.Context{IncusProject: "project", YardInstanceName: "yard"}, true)
			if !reflect.DeepEqual(result, map[string]string{"sample": test.want}) {
				t.Fatalf("health = %#v", result)
			}
		})
	}
}

func TestIntegrationHealthDoesNotProbeStoppedOrInvalidCommands(t *testing.T) {
	for _, test := range []struct {
		command string
		running bool
	}{
		{command: "sample-health", running: false},
		{command: "sample-health; echo private", running: true},
	} {
		runtime := Runtime{
			Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": "sample", "AGENT_sample_HEALTH": test.command},
			Executor: statusExecutorFunc(func(context.Context, string, string, ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
				t.Fatal("unsafe health probe executed")
				return ports.InstanceExecResult{}, nil
			}),
		}
		if got := runtime.ReadIntegrationHealth(context.Background(), domain.Context{}, test.running); got["sample"] != "unknown" {
			t.Fatalf("health = %#v", got)
		}
	}
}

func TestIntegrationHealthTimeoutIsUnknown(t *testing.T) {
	runtime := Runtime{
		Environment:  map[string]string{"CODING_TOOL_INTEGRATIONS": "sample", "AGENT_sample_HEALTH": "sample-health"},
		ProbeTimeout: time.Millisecond,
		Executor: statusExecutorFunc(func(ctx context.Context, _, _ string, _ ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
			<-ctx.Done()
			return ports.InstanceExecResult{Stdout: []byte(`{"state":"ready"}`)}, nil
		}),
	}
	if got := runtime.ReadIntegrationHealth(context.Background(), domain.Context{}, true); got["sample"] != "unknown" {
		t.Fatalf("timed-out probe reported %#v", got)
	}
}

func TestIntegrationHealthSupportsExecutablePaths(t *testing.T) {
	executor := &spaceExecutorStub{result: ports.InstanceExecResult{Stdout: []byte(`{"state":"ready"}`)}}
	runtime := Runtime{
		Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": "sample", "AGENT_sample_HEALTH": "/usr/local/bin/sample-health"},
		Executor:    executor,
	}
	if got := runtime.ReadIntegrationHealth(context.Background(), domain.Context{}, true); got["sample"] != "ready" ||
		len(executor.calls) != 1 || executor.calls[0].Command[0] != "/usr/local/bin/sample-health" {
		t.Fatalf("configured executable path was not used: %#v, %#v", got, executor.calls)
	}
}

func TestAgentStatusReportsBackgroundStartup(t *testing.T) {
	runtime := Runtime{
		Environment: map[string]string{"CODING_TOOL_INTEGRATIONS": "sample", "AGENT_sample_HEALTH": "sample-health"},
		Executor:    &spaceExecutorStub{result: ports.InstanceExecResult{Stdout: []byte(`{"state":"starting"}`)}},
	}
	got := runtime.agentStatus(context.Background(), domain.Context{}, true)
	if len(got) != 1 || got[0].State != "starting" || got[0].URL != "" {
		t.Fatalf("background startup reported as available: %#v", got)
	}
}
