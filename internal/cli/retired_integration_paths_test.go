package cli

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

const (
	retiredReminder = "retired integration widget may still be installed in yard default; run: yard -Y default init"
	retiredProofKey = "user.subyard.retired_integration.widget"
)

// retiredYardFixture selects a synthetic retired package for the default yard.
func retiredYardFixture(t *testing.T) (string, []string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	packageRoot := filepath.Join(root, "config", "retired-integrations", "widget")
	if err := os.MkdirAll(packageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(packageRoot, "integration.json"), `{"schema_version":1,"retired":true,"lifecycle":{"handler":"retire.py","instance_config_keys":[],"instance_devices":[]}}`, 0o600)
	writeCLIFile(t, filepath.Join(packageRoot, "retire.py"), "#!/bin/sh\n", 0o700)
	writeCLIFile(t, filepath.Join(root, "config", "projects-changed.sh"), "#!/bin/sh\nexit 0\n", 0o755)
	settings := filepath.Join(root, "state", "yards", "default", "config.env")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, settings, "CODING_TOOL_INTEGRATIONS='widget'\n", 0o600)
	return root, environment
}

func retiredInstance(status string, proven bool) ports.InstanceInfo {
	instance := lifecycleIncus().Instances["subyard/yard"]
	instance.Status = status
	instance.Config, instance.LocalConfig = maps.Clone(instance.Config), maps.Clone(instance.LocalConfig)
	if instance.Config == nil {
		instance.Config = map[string]string{}
	}
	if instance.LocalConfig == nil {
		instance.LocalConfig = map[string]string{}
	}
	if proven {
		instance.Config[retiredProofKey], instance.LocalConfig[retiredProofKey] = "true", "true"
	}
	return instance
}

// An ordinary update has no pending source migration, so only the real activation
// observation can carry the reminder for a stopped yard into --check and --yes.
func TestUpdateWarnsAboutStoppedRetiredYardWithoutSourceMigration(t *testing.T) {
	for _, test := range []struct {
		name   string
		status string
		proven bool
		want   []string
	}{
		{"stopped without proof", "Stopped", false, []string{retiredReminder}},
		{"stopped with proof", "Stopped", true, nil},
		{"running without proof", "Running", false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment := retiredYardFixture(t)
			instance := retiredInstance(test.status, test.proven)
			fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": instance}}
			for range 8 {
				fake.ExecSteps = append(fake.ExecSteps, testkit.IncusExecStep{Result: ports.InstanceExecResult{ExitCode: 0}})
			}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Environment: environment, WorkingDir: root,
				InitPlatform: newInitPlatformFixture(), Incus: fake, Executor: fake, Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			configHome := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			links := releasetransition.ReleaseLinks{Active: "release-a"}
			options := releasetransition.V2Options{ConfigHome: configHome,
				Releases: releasetransition.ReleasePair{From: "release-a", Target: "release-a"},
				ObserveLinks: func(context.Context) (releasetransition.ReleaseLinks, error) {
					return links, nil
				}, RegistryPayload: []byte(`{"schemaVersion":2,"minimumEpochs":{"settings":1},"currentEpochs":{"settings":1},"migrations":[]}`),
				ArtifactDigest: releasetransition.Fingerprint(strings.Repeat("a", 64)),
				Reconcilers:    []releasetransition.V2ActivationReconciler{&materializedConfigActivationReconciler{cli: program, configHome: configHome}},
				VerifyAuthorization: func(releasetransition.PlanToken, releasetransition.Authorization) bool {
					return false
				},
			}
			transition, err := releasetransition.NewV2Transition(options)
			if err != nil {
				t.Fatal(err)
			}
			goal := releasetransition.Goal{Target: "release-a", Direction: releasetransition.DirectionActivateTarget}
			inspection, err := transition.Inspect(context.Background(), goal)
			if err != nil {
				t.Fatal(err)
			}
			if inspection.Outcome == nil || !slices.Equal(inspection.Outcome.Warnings, test.want) {
				t.Fatalf("update --check outcome=%+v, want warnings %q", inspection.Outcome, test.want)
			}
			if test.status == "Stopped" && len(fake.ExecCalls) != 0 {
				t.Fatalf("update observation entered the stopped guest: %+v", fake.ExecCalls)
			}
			stdout.Reset()
			program.printUpdateResult(&releaseExecution{prepared: releaseruntime.Prepared{Action: "update.activate"}, inspection: &inspection}, true)
			if got := strings.Count(stdout.String(), "Warning: "+retiredReminder); got != len(test.want) {
				t.Fatalf("update --yes result=%q", stdout.String())
			}
			if instanceAfter := fake.Instances["subyard/yard"]; instanceAfter.Status != test.status {
				t.Fatalf("update changed yard power: %s", instanceAfter.Status)
			}
		})
	}
}

func TestStartWarnsThroughTheRealCommandUntilProven(t *testing.T) {
	for _, test := range []struct {
		name    string
		initial string
		proven  bool
		warned  bool
	}{
		{"stopped without proof", "Stopped", false, true},
		{"stopped with proof", "Stopped", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, environment := retiredYardFixture(t)
			runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Result: domain.AdapterResult{
				Schema: 1, OperationID: "operation-start", Status: "ok",
			}}}}
			incus := lifecycleIncus()
			incus.Instances["subyard/yard"] = retiredInstance(test.initial, test.proven)
			var output bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"start"},
				Environment: append(environment, "SUBYARD_OPERATION_ID=operation-start"), WorkingDir: root,
				Stdout: &output, Stderr: &output, AdapterRunner: runner, Prompt: &testkit.Prompt{},
				Incus: powerScriptedIncus(runner, incus), NetworkPolicy: allowTestNetworkPolicy()})
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code != 0 {
				t.Fatalf("start code=%d output=%q", code, output.String())
			}
			if len(runner.Requests) != 1 || runner.Requests[0].Action != "start" {
				t.Fatalf("start adapter requests=%+v", runner.Requests)
			}
			if got := strings.Count(output.String(), "Warning: "+retiredReminder+"\n"); (got == 1) != test.warned || got > 1 {
				t.Fatalf("start output=%q", output.String())
			}
		})
	}
}

// Native init replaces its preview with operation steps, so only the real prepared
// command proves that a stopped yard's retirement consent is shown before confirmation.
func TestInitPlanNamesStoppedRetirementThroughTheRealCommand(t *testing.T) {
	const retirement = "[do  ] yard default: temporarily start this stopped yard to retire integration widget, then restore its desired power"
	for _, proven := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "proven"}[proven], func(t *testing.T) {
			root, environment := retiredYardFixture(t)
			incus := lifecycleIncus()
			instance := retiredInstance("Stopped", proven)
			incus.Instances["subyard/yard"] = instance
			incus.Reconcile = ports.ReconcileState{InstanceFound: true, Instance: instance}
			var output bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"init"},
				Environment: append(environment, "SUBYARD_OPERATION_ID=operation-init"), WorkingDir: root,
				Stdin: strings.NewReader(""), Stdout: &output, Stderr: &output, Incus: incus, Executor: incus,
				NetworkPolicy: allowTestNetworkPolicy()})
			if err != nil {
				t.Fatal(err)
			}
			code := program.Run(context.Background())
			t.Logf("code=%d output=%s", code, output.String())
			if strings.Count(output.String(), retirement) != map[bool]int{false: 1, true: 0}[proven] {
				t.Fatalf("code=%d output=%q", code, output.String())
			}
			if len(incus.ExecCalls) != 0 || incus.Instances["subyard/yard"].Status != "Stopped" {
				t.Fatalf("unconfirmed init touched the stopped yard: %+v", incus.ExecCalls)
			}
		})
	}
}
