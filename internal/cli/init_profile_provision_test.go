package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/hostruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestInitProfileProvisionsSelectedHook(t *testing.T) {
	for _, scenario := range []string{"existing", "fresh", "declined", "failed", "address changed"} {
		t.Run(scenario, func(t *testing.T) {
			cli, loaded, path, runner := provisionEndpointFixture(t)
			delete(cli.baseEnv, "AGENTS")
			delete(cli.env, "AGENTS")
			delete(cli.baseEnv, "CODING_TOOL_INTEGRATIONS")
			delete(cli.env, "CODING_TOOL_INTEGRATIONS")
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			preset := filepath.Join(cli.options.RepositoryRoot, "config", "profiles", "sample", "yard.env")
			// Empty endpoint placeholders permit a retry after automatic discovery.
			content = append(content, []byte("CODING_TOOL_INTEGRATIONS=\nRESOURCE_RELAY_IPV4=\nRESOURCE_RELAY_INTERFACE=\n")...)
			writeCLIFile(t, preset, string(content), 0600)
			writeCLIFile(t, path, string(content), 0600)
			if scenario == "fresh" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			cli.options.InitPlatform = newInitPlatformFixture()
			prompt := &testkit.Prompt{Answers: []bool{scenario != "declined"}}
			cli.options.Prompt = prompt
			cli.options.Arguments = []string{"-Y", loaded.Context.YardName, "init", "--profile", "sample"}
			var stdout, stderr bytes.Buffer
			cli.options.Stdout, cli.options.Stderr = &stdout, &stderr
			options := cli.options
			options.Environment = withoutCommandSetting(withoutCommandSetting(options.Environment, "AGENTS"), "CODING_TOOL_INTEGRATIONS")
			addresses := cli.provisionEndpointAddresses
			cli, err = New(options)
			if err != nil {
				t.Fatal(err)
			}
			cli.provisionEndpointAddresses = addresses
			if scenario == "address changed" {
				reads := 0
				cli.provisionEndpointAddresses = func() ([]hostruntime.OwnerIPv4, error) {
					reads++
					if reads >= 3 {
						return nil, nil
					}
					return addresses()
				}
			}
			runner.Steps = []testkit.AdapterStep{
				{Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Stderr: "changed"},
				{Result: domain.AdapterResult{Schema: 1, Status: "ok"}},
				{Result: domain.AdapterResult{Schema: 1, Status: "ok"}, Stderr: "converged"},
			}
			if scenario == "failed" {
				runner.Steps[1].Err = errors.New("install failed")
			}
			code := cli.Run(context.Background())
			want := 0
			if scenario == "declined" || scenario == "failed" || scenario == "address changed" {
				want = 1
			}
			if code != want {
				t.Fatalf("code=%d want=%d stdout=%s stderr=%s", code, want, stdout.String(), stderr.String())
			}
			stored, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "declined" || scenario == "failed" || scenario == "address changed" {
				if strings.Contains(string(stored), "8.8.8.8") {
					t.Fatal("saved endpoint without successful provisioning")
				}
			} else {
				if !strings.Contains(string(stored), "8.8.8.8") {
					t.Fatalf("endpoint missing: %s", stored)
				}
				retry, err := New(options)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := retry.loadInitContext(loaded.Context.YardName, true, []string{"--profile", "sample"}); err != nil {
					t.Fatalf("retry rejects discovered endpoint: %v", err)
				}
			}
			if len(prompt.Seen) != 1 {
				t.Fatalf("prompts=%d", len(prompt.Seen))
			}
			for _, request := range runner.Requests {
				if request.Adapter != "provision" {
					t.Fatalf("unexpected resource mutation: %+v", request)
				}
				if request.Action == "profile" && (len(request.Arguments) != 1 || request.Arguments[0] != "sample") {
					t.Fatalf("unexpected profiles: %v", request.Arguments)
				}
			}
			if scenario == "declined" && len(runner.Requests) != 0 {
				t.Fatal("decline ran provision")
			}
		})
	}
}

func TestInitProfileDefersOnlyUnavailableInstanceAssessment(t *testing.T) {
	cli, loaded, path, _ := provisionEndpointFixture(t)
	incus := cli.options.Incus.(*testkit.Incus)
	incus.Err = ports.ErrIncusUnavailable
	execution := &initExecution{loaded: loaded, plan: application.ReconcilePlan{Steps: []application.ReconcileStep{{Stage: application.ReconcileStage{ID: ports.ReconcileStageIncus}}}}}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.prepareInitProfileProvision(context.Background(), loaded, execution, []string{"--profile", "sample"}); err != nil {
		t.Fatalf("blocked recoverable init: %v", err)
	}
	_, err = cli.executeInitProfileProvision(context.Background(), execution, &application.Orchestrator{}, domain.OperationPlan{}, &bytes.Buffer{})
	if !errors.Is(err, ports.ErrIncusUnavailable) {
		t.Fatalf("did not require instance check after reconciliation: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("persisted endpoint without instance ownership check")
	}
	execution.profileProvision.endpoint.readAddresses = func() ([]hostruntime.OwnerIPv4, error) { return nil, nil }
	if err := cli.observeInitProfileProvision(context.Background(), execution); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("deferred address stale check: %v", err)
	}
}

func TestInitProfileAcceptsPersistedEndpointPort(t *testing.T) {
	for _, test := range []struct {
		name      string
		fresh     bool
		override  string
		wantError bool
	}{
		{name: "persisted custom port"},
		{name: "matching command value", override: "43000"},
		{name: "conflicting command value", override: "44000", wantError: true},
		{name: "fresh command override", fresh: true, override: "43000", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			program, loaded, path, _ := provisionEndpointFixture(t)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			preset := filepath.Join(program.options.RepositoryRoot, "config", "profiles", "sample", "yard.env")
			writeCLIFile(t, preset, string(content), 0o600)
			if test.fresh {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeCLIFile(t, path, strings.Replace(string(content), "RESOURCE_RELAY_PORT=42000", "RESOURCE_RELAY_PORT=43000", 1), 0o600)
			}
			options := program.options
			if test.override != "" {
				options.Environment = append(options.Environment, "RESOURCE_RELAY_PORT="+test.override)
			}
			program, err = New(options)
			if err != nil {
				t.Fatal(err)
			}
			got, _, err := program.loadInitContext(loaded.Context.YardName, true, []string{"--profile", "sample"})
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "command environment overrides profile \"sample\" at setting RESOURCE_RELAY_PORT") {
					t.Fatalf("override error=%v", err)
				}
			} else if err != nil || got.Environment["RESOURCE_RELAY_PORT"] != "43000" {
				t.Fatalf("port=%q error=%v", got.Environment["RESOURCE_RELAY_PORT"], err)
			}
		})
	}
}
