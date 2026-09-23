package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/testkit"
)

type integrationCleanupFixture struct {
	*integrationRuntimeFixture
	plan       reconcileruntime.IntegrationCleanupPlan
	planErr    error
	applyErr   error
	planned    int
	applied    int
	afterApply func()
}

func (fixture *integrationCleanupFixture) IntegrationCleanupPlan(context.Context, string) (reconcileruntime.IntegrationCleanupPlan, error) {
	fixture.planned++
	return fixture.plan, fixture.planErr
}

func (fixture *integrationCleanupFixture) ApplyIntegrationCleanup(_ context.Context, _ string, plan reconcileruntime.IntegrationCleanupPlan) error {
	fixture.applied++
	if plan.Fingerprint != fixture.plan.Fingerprint {
		return errors.New("cleanup plan mismatch")
	}
	if fixture.afterApply != nil {
		fixture.afterApply()
	}
	return fixture.applyErr
}

func cleanupCLIFixture(t *testing.T, selection string) (*CLI, *integrationCleanupFixture, string, *bytes.Buffer) {
	t.Helper()
	cli, _, integration, selectionPath, output := integrationFixture(t, selection)
	cleanupPath := filepath.Join(cli.options.RepositoryRoot, "config", "agents", "codex", "cleanup.sh")
	if err := os.MkdirAll(filepath.Dir(cleanupPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, cleanupPath, "#!/bin/sh\n", 0o600)
	agents := filepath.Join(cli.options.RepositoryRoot, "config", "agents.env")
	content, err := os.ReadFile(agents)
	if err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, agents, string(content)+"AGENT_codex_CLEANUP="+cleanupPath+"\n", 0o600)
	fixture := &integrationCleanupFixture{
		integrationRuntimeFixture: integration,
		plan: reconcileruntime.IntegrationCleanupPlan{
			Fingerprint: strings.Repeat("a", 64), Changed: true,
			Steps: []string{"Disable and archive the integration-owned service"},
		},
	}
	cli.options.IntegrationRuntime = func(loaded config.Loaded) IntegrationRuntime {
		integration.requested = append([]string(nil), loaded.Integrations.Requested...)
		return fixture
	}
	return cli, fixture, selectionPath, output
}

func TestIntegrationCleanupCheckAllowsSourceManagedDisabledIntegration(t *testing.T) {
	cli, runtime, path, output := cleanupCLIFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	configHome := filepath.Join(cli.options.RepositoryRoot, "state")
	if err := configsync.RegisterSource(configHome, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareIntegrationTest(t, cli, "cleanup", "codex", "--check")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if code := cli.runPreparedCommand(context.Background(), prepared, false); code != 0 {
		t.Fatalf("check exit=%d output=%s", code, output)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("cleanup check changed selection: %q, %v", after, err)
	}
	if runtime.planned != 1 || runtime.applied != 0 {
		t.Fatalf("check planned=%d applied=%d", runtime.planned, runtime.applied)
	}
	if !strings.Contains(output.String(), "Integration cleanup: codex") ||
		!strings.Contains(output.String(), runtime.plan.Steps[0]) {
		t.Fatalf("cleanup preview missing: %s", output)
	}
}

func TestIntegrationCleanupConfirmedPreservesSelection(t *testing.T) {
	cli, runtime, path, output := cleanupCLIFixture(t, "# operator choice\nCODING_TOOL_INTEGRATIONS=\n")
	before, _ := os.ReadFile(path)
	prepared, err := prepareIntegrationTest(t, cli, "cleanup", "codex", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 0 {
		t.Fatalf("cleanup exit=%d output=%s", code, output)
	}
	after, _ := os.ReadFile(path)
	if runtime.applied != 1 || !bytes.Equal(before, after) {
		t.Fatalf("cleanup applied=%d selection before=%q after=%q", runtime.applied, before, after)
	}
}

func TestIntegrationCleanupEOFDeclinesWithoutApply(t *testing.T) {
	cli, runtime, path, _ := cleanupCLIFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	before, _ := os.ReadFile(path)
	prepared, err := prepareIntegrationTest(t, cli, "cleanup", "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if code := cli.runPreparedCommand(context.Background(), prepared, false); code == 0 {
		t.Fatal("EOF accepted cleanup")
	}
	after, _ := os.ReadFile(path)
	if runtime.applied != 0 || !bytes.Equal(before, after) {
		t.Fatal("declined cleanup changed runtime or selection")
	}
}

func TestIntegrationCleanupRejectsSelectedRequiredAndStopped(t *testing.T) {
	for _, test := range []struct {
		name, selection, want string
		stopped               bool
	}{
		{name: "selected", selection: "CODING_TOOL_INTEGRATIONS=codex\n", want: "still selected"},
		{name: "dependency", selection: "CODING_TOOL_INTEGRATIONS=paseo\n", want: "required by another"},
		{name: "stopped", selection: "CODING_TOOL_INTEGRATIONS=\n", want: "running yard", stopped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cli, runtime, _, _ := cleanupCLIFixture(t, test.selection)
			if test.stopped {
				incus, _ := cli.statusPorts()
				fixture := incus.(*testkit.Incus)
				instance := fixture.Instances["subyard/yard"]
				instance.Status = "Stopped"
				fixture.Instances["subyard/yard"] = instance
			}
			prepared, err := prepareIntegrationTest(t, cli, "cleanup", "codex")
			if err == nil || prepared != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("prepared=%v err=%v", prepared, err)
			}
			if runtime.applied != 0 {
				t.Fatal("rejected cleanup applied")
			}
		})
	}
}

func TestIntegrationCleanupRejectsChangedConfiguration(t *testing.T) {
	cli, runtime, _, output := cleanupCLIFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
	prepared, err := prepareIntegrationTest(t, cli, "cleanup", "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	writeCLIFile(t, filepath.Join(cli.options.RepositoryRoot, "state", "config.env"),
		"AGENT_codex_CHECK=changed-check\n", 0o600)
	if code := cli.runPreparedCommand(context.Background(), prepared, true); code != 1 ||
		!strings.Contains(output.String(), "settings changed before cleanup") {
		t.Fatalf("stale cleanup exit/output: %d %s", code, output)
	}
	if runtime.applied != 0 {
		t.Fatal("stale cleanup applied")
	}
}

func TestIntegrationCleanupRejectsMissingCapabilityAndUnknownID(t *testing.T) {
	t.Run("missing runtime capability", func(t *testing.T) {
		cli, _, _, _, _ := integrationFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
		prepared, err := prepareIntegrationTest(t, cli, "cleanup", "codex")
		if err == nil || prepared != nil || !strings.Contains(err.Error(), "does not support") {
			t.Fatalf("prepared=%v err=%v", prepared, err)
		}
	})
	t.Run("unknown ID", func(t *testing.T) {
		cli, runtime, _, _ := cleanupCLIFixture(t, "CODING_TOOL_INTEGRATIONS=\n")
		prepared, err := prepareIntegrationTest(t, cli, "cleanup", "unknown")
		if err == nil || prepared != nil || !strings.Contains(err.Error(), "unknown integration") {
			t.Fatalf("prepared=%v err=%v", prepared, err)
		}
		if runtime.planned != 0 {
			t.Fatal("unknown integration reached cleanup runtime")
		}
	})
}
