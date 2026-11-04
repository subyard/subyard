package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func TestMigrationHelpersRequireOwningParentBeforeSetup(t *testing.T) {
	for _, helper := range []struct {
		action, yard  string
		parent, wrong func(context.Context) context.Context
	}{
		{"reconcile-power-reconciler", testyardmigration.LegacyYard, ownerMigrationChildContext, routeMigrationChildContext},
		{"reconcile-test-vm-broker", testyardmigration.CurrentYard, routeMigrationChildContext, ownerMigrationChildContext},
	} {
		t.Run(helper.action, func(t *testing.T) {
			home := testkit.TempDir(t)
			configHome := filepath.Join(home, "config")
			if err := os.MkdirAll(filepath.Join(configHome, "yards", helper.yard), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(configHome, "yards", helper.yard, "config.env"), "YARD_TEMPLATE='unfinished\n", 0o600)
			platform := newInitPlatformFixture()
			platform.converged[ports.ReconcileStageTestVMs] = false
			var diagnostics bytes.Buffer
			program, err := New(Options{RepositoryRoot: repositoryRoot(t), Arguments: []string{"-Y", helper.yard, "_migrate", helper.action},
				Environment: []string{"HOME=" + home, "SUBYARD_OPERATOR_HOME=" + home, "SUBYARD_CONFIG_HOME=" + configHome, "SUBYARD_HOME=" + filepath.Join(home, "data"), "SUBYARD_INTERNAL_MIGRATION_CHILD=1"},
				Stderr:      &diagnostics, InitPlatform: platform})
			if err != nil {
				t.Fatal(err)
			}
			// The malformed config proves refusal happens before configuration setup.
			for name, ctx := range map[string]context.Context{"environment only": context.Background(), "other parent": helper.wrong(context.Background())} {
				diagnostics.Reset()
				if code := program.Run(ctx); code == 0 || len(platform.applied) != 0 || !strings.Contains(diagnostics.String(), "requires its owning parent") {
					t.Fatalf("%s authorized hidden effect: code=%d applied=%v diagnostics=%q", name, code, platform.applied, diagnostics.String())
				}
			}
			if err := validateMigrationChildInvocation(helper.parent(context.Background()), helper.yard, []string{"--yes", "_migrate", helper.action}); err == nil {
				t.Fatal("consent injected into the authorized tuple")
			}
			if err := validateMigrationChildInvocation(helper.parent(context.Background()), "other", []string{"_migrate", helper.action}); err == nil {
				t.Fatal("authority crossed to another yard")
			}
			if err := validateMigrationChildInvocation(helper.parent(context.Background()), helper.yard, []string{"_migrate", helper.action}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrationChildReadsTestVMsWithoutServiceRegistration(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
		Schema: 1, OperationID: "migration-child-test-vms-status", Status: "ok",
	}}}}
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status = "Running"
	incus.Instances["subyard/yard"] = instance
	// The fresh child environment drops overrides; the yard selects its backend by its settings.
	settings := filepath.Join(environmentValue(environment, "SUBYARD_CONFIG_HOME"), "yards", "default", "config.env")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, settings, "NESTED_E2E_VMS=1\n", 0o600)
	var stderr bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard",
		Environment: append(environment, "SUBYARD_OPERATION_ID=migration-child-test-vms-status"),
		WorkingDir:  root, Stderr: &stderr, AdapterRunner: runner, Incus: incus})
	if err != nil {
		t.Fatal(err)
	}
	if err := program.runReleaseTransitionYardCommand(context.Background(), "default", &stderr, "test-vms", "status"); err != nil || len(runner.Requests) != 1 {
		t.Fatalf("migration child needed a registered service: err=%v requests=%d stderr=%q", err, len(runner.Requests), stderr.String())
	}
}
