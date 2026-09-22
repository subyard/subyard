package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/testkit"
)

type integrationRuntimeFixture struct {
	plan       reconcileruntime.IntegrationPlan
	applyErr   error
	afterApply func()
	applied    int
	requested  []string
}

func (fixture *integrationRuntimeFixture) IntegrationPlan(context.Context) (reconcileruntime.IntegrationPlan, error) {
	return fixture.plan, nil
}
func (fixture *integrationRuntimeFixture) ApplyIntegrations(context.Context, reconcileruntime.IntegrationPlan) error {
	fixture.applied++
	if fixture.afterApply != nil {
		fixture.afterApply()
	}
	return fixture.applyErr
}
func integrationFixture(t *testing.T, selection string) (*CLI, *testkit.Incus, *integrationRuntimeFixture, string, *bytes.Buffer) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	manifestPath := filepath.Join(root, "config", "commands.registry")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, manifestPath, string(manifest)+"integration||@integration||forward|mutate|dynamic|public|lifecycle|simple|integration <command>|integrations|--json --yes --help|enable disable status\n", 0600)
	writeCLIFile(t, filepath.Join(root, "config", "agents.env"), "CODING_TOOL_INTEGRATIONS=\nAGENT_codex_COMMAND=codex\nAGENT_paseo_COMMAND=paseo\nAGENT_paseo_DEPENDS=codex\n", 0600)
	path := filepath.Join(root, "state", "yards", "default", "config.env")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, path, selection, 0600)
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	runtime := &integrationRuntimeFixture{plan: reconcileruntime.IntegrationPlan{Fingerprint: "stable", Changed: true, Steps: []string{"Reconcile owned integrations"}}}
	var output bytes.Buffer
	cli, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus, Stdin: strings.NewReader(""), Stdout: &output, Stderr: &output, IntegrationRuntime: func(loaded config.Loaded) IntegrationRuntime {
		runtime.requested = slices.Clone(loaded.Integrations.Requested)
		return runtime
	}})
	if err != nil {
		t.Fatal(err)
	}
	return cli, incus, runtime, path, &output
}
func prepareIntegrationTest(t *testing.T, cli *CLI, arguments ...string) (*preparedCommand, error) {
	t.Helper()
	loaded, err := cli.loadContext("default")
	if err != nil {
		return nil, err
	}
	definition, _ := cli.manifest.Lookup("integration")
	return cli.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, Arguments: arguments, ExplicitYard: true})
}
func TestIntegrationStoppedAndMissingRejectBeforePlanOrWrite(t *testing.T) {
	for _, state := range []string{"Stopped", "missing"} {
		t.Run(state, func(t *testing.T) {
			cli, incus, runtime, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=codex\n")
			if state == "missing" {
				delete(incus.Instances, "subyard/yard")
			} else {
				instance := incus.Instances["subyard/yard"]
				instance.Status = state
				incus.Instances["subyard/yard"] = instance
			}
			before, _ := os.ReadFile(path)
			prepared, err := prepareIntegrationTest(t, cli, "enable", "codex")
			if err == nil || prepared != nil || !strings.Contains(err.Error(), "running yard") {
				t.Fatalf("prepared=%v err=%v", prepared, err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) || runtime.applied != 0 {
				t.Fatal("stopped yard mutated")
			}
		})
	}
}
func TestIntegrationDependencyDisableAndTemporaryOverrideRejected(t *testing.T) {
	cli, _, _, _, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=paseo\n")
	if _, err := prepareIntegrationTest(t, cli, "disable", "codex"); err == nil || !strings.Contains(err.Error(), "required by paseo") {
		t.Fatalf("dependency error=%v", err)
	}
	cli, _, _, _, _ = integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	cli.baseEnv["CODING_TOOL_INTEGRATIONS"] = "codex"
	cli.env["CODING_TOOL_INTEGRATIONS"] = "codex"
	if _, err := prepareIntegrationTest(t, cli, "enable", "codex"); err == nil || !strings.Contains(err.Error(), "temporary") {
		t.Fatalf("override error=%v", err)
	}
}
func TestIntegrationSourceGuardPreventsWrite(t *testing.T) {
	cli, _, _, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	if err := configsync.RegisterSource(filepath.Join(cli.options.RepositoryRoot, "state"), filepath.Join(cli.options.RepositoryRoot, "checkout")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := prepareIntegrationTest(t, cli, "enable", "codex"); err == nil || !strings.Contains(err.Error(), "source-managed") {
		t.Fatalf("guard=%v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("source-managed file changed")
	}
}
func TestIntegrationDesiredPersistsOnApplyFailureAndRetry(t *testing.T) {
	cli, _, runtime, path, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	prepared, err := prepareIntegrationTest(t, cli, "enable", "codex")
	if err != nil {
		t.Fatal(err)
	}
	runtime.applyErr = errors.New("injected package failure")
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 1 {
		t.Fatalf("exit=%d %s", code, output)
	}
	prepared.Close()
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "codex") {
		t.Fatalf("lost desired: %s %s", content, output)
	}
	runtime.applyErr = nil
	// A new invocation captures the freshly persisted desired set.
	delete(cli.env, "CODING_TOOL_INTEGRATIONS")
	delete(cli.env, "AGENTS")
	prepared, err = prepareIntegrationTest(t, cli, "enable", "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 0 || runtime.applied != 2 {
		t.Fatalf("retry=%d calls=%d %s", code, runtime.applied, output)
	}
}
func TestIntegrationStoppedAfterPlanAndCASRacePreserveDesired(t *testing.T) {
	for _, failure := range []string{"stopped", "cas", "runtime"} {
		t.Run(failure, func(t *testing.T) {
			cli, incus, runtime, path, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
			prepared, err := prepareIntegrationTest(t, cli, "enable", "codex")
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			switch failure {
			case "stopped":
				instance := incus.Instances["subyard/yard"]
				instance.Status = "Stopped"
				incus.Instances["subyard/yard"] = instance
			case "cas":
				writeCLIFile(t, path, "CODING_TOOL_INTEGRATIONS=paseo\n", 0600)
			case "runtime":
				runtime.plan.Fingerprint = "changed"
			}
			if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 1 {
				t.Fatalf("exit=%d %s", code, output)
			}
			if runtime.applied != 0 {
				t.Fatal("stale operation applied")
			}
			content, _ := os.ReadFile(path)
			if strings.Contains(string(content), "codex") {
				t.Fatalf("stale intent written: %s", content)
			}
		})
	}
}
func TestIntegrationStatusStoppedReadOnly(t *testing.T) {
	cli, incus, _, path, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Stopped"
	incus.Instances["subyard/yard"] = instance
	loaded, err := cli.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	status, err := cli.queryIntegrationStatus(context.Background(), loaded, "")
	if err != nil || status.Observed != "stopped" || !status.Selection.Present || len(status.Selection.Requested) != 0 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "generated", "integration-locks", "default.lock")); !os.IsNotExist(err) {
		t.Fatalf("status acquired lock: %v", err)
	}
}

func TestIntegrationConcurrentHostSettingsChangeCannotReportReady(t *testing.T) {
	cli, _, runtime, path, output := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	prepared, err := prepareIntegrationTest(t, cli, "enable", "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	runtime.afterApply = func() {
		writeCLIFile(t, filepath.Join(cli.options.RepositoryRoot, "state", "config.env"), "AGENT_codex_CHECK=changed-check\n", 0600)
	}
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 1 || !strings.Contains(output.String(), "configuration changed during") {
		t.Fatalf("concurrent settings incorrectly reported ready: %d %s", code, output)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), "codex") {
		t.Fatalf("lost saved intent: %q %v", content, err)
	}
}
