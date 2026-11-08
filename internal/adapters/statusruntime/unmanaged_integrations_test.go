package statusruntime

import (
	"context"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

func TestUnmanagedIntegrationObservationsAreAdvisoryAndProfileOwned(t *testing.T) {
	for _, scenario := range []string{"present", "selected", "stopped", "profile-absent", "invalid", "private", "oversized", "failed"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			runtime := Runtime{Environment: map[string]string{"ENVIRONMENT_PROFILES": "provider"}, Profiles: []profile.Definition{{Name: "provider", UnmanagedIntegrations: []profile.UnmanagedIntegration{{ID: "sample", Command: []string{"sample-probe"}, Detail: "unmanaged fixture"}}}}}
			if scenario == "selected" {
				runtime.Environment["CODING_TOOL_INTEGRATIONS"] = "sample"
			}
			if scenario == "profile-absent" {
				runtime.Environment["ENVIRONMENT_PROFILES"] = ""
			}
			runtime.Executor = statusExecutorFunc(func(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
				calls++
				if _, ok := ctx.Deadline(); !ok || len(request.Command) != 3 || request.Command[1] != "/home/fixture" || request.Command[2] != "1234" {
					t.Fatalf("unbound probe: %#v", request)
				}
				output := `{"state":"unmanaged"}`
				switch scenario {
				case "invalid":
					output += ` {"state":"absent"}`
				case "private":
					output = `{"state":"unmanaged","detail":"private"}`
				case "oversized":
					output += string(make([]byte, 1024))
				case "failed":
					return ports.InstanceExecResult{ExitCode: 1}, nil
				}
				return ports.InstanceExecResult{Stdout: []byte(output)}, nil
			})
			result := runtime.ReadUnmanagedIntegrations(context.Background(), domain.Context{DevUser: "fixture", DevUID: 1234}, scenario != "stopped")
			if (result["sample"] == "unmanaged fixture") != (scenario == "present") {
				t.Fatalf("classification: %#v", result)
			}
			if (scenario == "selected" || scenario == "stopped" || scenario == "profile-absent") && calls != 0 {
				t.Fatal("inactive probe ran")
			}
		})
	}
}
