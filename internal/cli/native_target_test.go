package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/uninstall"
)

func nativeTargetFixture(t *testing.T) (*CLI, *testkit.Incus, string) {
	t.Helper()
	root, _, _ := nativeFixture(t)
	home := filepath.Join(root, "home")
	configHome := filepath.Join(root, "state")
	environment := []string{"HOME=" + home, "SUBYARD_OPERATOR_HOME=" + home, "SUBYARD_CONFIG_HOME=" + configHome, "SUBYARD_HOME=" + root + "/data", "SUBYARD_NO_AUDIT=1"}
	for _, name := range []string{"commands.registry", "projects-changed.sh"} {
		content := mustReadConfigFixture(t, filepath.Join(repositoryRoot(t), "config", name))
		testkit.WriteFile(t, filepath.Join(root, "config", name), content, 0644)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/incus.project.env"), []byte("SRV_VOLUME=yard-srv\n"), 0644)
	writeEmptyNamedFile(t, filepath.Join(configHome, "config.env"), "INCUS_PROJECT=custom\nHOST_BASE="+home+"/host\nRESTRICTED_DISK_PATHS="+home+"/host\n", 0600)
	writeEmptyNamedFile(t, filepath.Join(configHome, "yards/named/config.env"), "# named\n", 0600)
	native := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	cli, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: native, Executor: native, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	return cli, native, configHome
}
func nativeInstance(owner string) ports.InstanceInfo {
	values := map[string]string{"volatile.uuid": "fixture-uuid", "user.subyard.name": owner}
	return ports.InstanceInfo{Project: "custom", Name: "yard-named", Type: domain.YardContainer, Config: values, LocalConfig: maps.Clone(values)}
}

func TestActivationAndProfileRefuseForeignMarkerBeforeGuest(t *testing.T) {
	cli, native, home := nativeTargetFixture(t)
	native.Instances["custom/yard-named"] = nativeInstance("foreign")
	loaded, err := cli.loadReleaseTransitionContext("named", home, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, activation := cli.observeInstalledReleaseTarget(context.Background(), configTarget{Name: "named", Loaded: loaded}, false)
	_, profile := (&profileRuntimeActivationReconciler{id: "sample-runtime"}).observeTarget(context.Background(), cli, configTarget{Name: "named", Loaded: loaded})
	for _, err := range []error{activation, profile} {
		var verdict *domain.NativeTargetRefusal
		if !errors.As(err, &verdict) || !strings.Contains(err.Error(), "belongs to yard foreign") {
			t.Fatalf("ownership cause lost: %v", err)
		}
	}
	if len(native.ExecCalls) != 0 {
		t.Fatal("foreign guest inspected")
	}
}

func TestNativeTargetChecksRetainIndependentCollisionAndForeignOwner(t *testing.T) {
	cli, native, home := nativeTargetFixture(t)
	native.Instances["custom/yard-named"] = nativeInstance("foreign")
	for _, name := range []string{"alpha", "beta"} {
		content := "INCUS_PROJECT=duplicate\nYARD_INSTANCE_NAME=duplicate\n"
		if name == "alpha" {
			content += "AGENT_codex_PROVISION=" + home + "/missing\n"
		}
		writeEmptyNamedFile(t, filepath.Join(home, "yards", name, "config.env"), content, 0600)
	}
	writeEmptyNamedFile(t, filepath.Join(home, "yards/broken/config.env"), "invalid assignment\n", 0600)
	loaded, err := cli.loadReleaseTransitionContext("named", home, nil)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	cli.options.Stderr = &diagnostics
	if code := cli.runConfig(context.Background(), loaded, []string{"status", "--all-local"}); code == 0 {
		t.Fatal("status accepted")
	}
	for _, want := range []string{"yards alpha and beta share native target", "belongs to yard foreign", "broken"} {
		if !strings.Contains(diagnostics.String(), want) {
			t.Fatalf("lost %q: %s", want, &diagnostics)
		}
	}
	if strings.Count(diagnostics.String(), "yard broken configuration:") != 1 {
		t.Fatal("duplicate load finding", &diagnostics)
	}
	_, targets, err := cli.releaseActivationTargets(context.Background(), "named", home, true, nil)
	if err == nil || !strings.Contains(err.Error(), "yards alpha and beta") || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("activation lost independent refusals: %v", err)
	}
	for _, target := range targets {
		if target.Name == "alpha" {
			t.Fatal("activation returned address-only target")
		}
	}
	if len(native.ExecCalls) != 0 {
		t.Fatal("foreign guest inspected")
	}
}

func TestSelectedActivationIgnoresUnrelatedLoadFailuresAndKeepsRemote(t *testing.T) {
	cli, _, home := nativeTargetFixture(t)
	writeEmptyNamedFile(t, filepath.Join(home, "yards/broken/config.env"), "invalid assignment\n", 0600)
	writeEmptyNamedFile(t, filepath.Join(home, "yards/remote/config.env"), "ACCESS_KIND=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=default\n", 0600)
	for _, name := range []string{"named", "remote"} {
		_, targets, err := cli.releaseActivationTargets(context.Background(), name, home, false, nil)
		if err != nil || len(targets) != 1 || targets[0].Name != name {
			t.Fatalf("selected %s lost its scope: %v %+v", name, err, targets)
		}
	}
	// Provisioning can be unreadable while scalar addresses still prove a collision.
	writeEmptyNamedFile(t, filepath.Join(home, "yards/alias/config.env"), "INCUS_PROJECT=custom\nYARD_INSTANCE_NAME=yard-named\nAGENT_codex_PROVISION="+home+"/missing\n", 0600)
	_, _, err := cli.releaseActivationTargets(context.Background(), "named", home, false, nil)
	if err == nil || !strings.Contains(err.Error(), "yards alias and named share native target") || strings.Contains(err.Error(), "broken") {
		t.Fatalf("selected address pair lost: %v", err)
	}
}

type collisionReadGuard struct {
	*testkit.Incus
	reads int
}

func (native *collisionReadGuard) Instance(context.Context, string, string) (ports.InstanceInfo, error) {
	native.reads++
	return ports.InstanceInfo{}, errors.New("collision assessment must not read Incus")
}

func TestNativeTargetGateAdmitsAddressRepair(t *testing.T) {
	cli, native, home := nativeTargetFixture(t)
	root := cli.options.RepositoryRoot
	environment := append(cli.options.Environment, "YARD_RUNTIME_ROOT="+filepath.Join(root, "runtime"))
	writeEmptyNamedFile(t, filepath.Join(home, "config.env"), "INCUS_PROJECT=custom\nYARD_INSTANCE_NAME=box\n", 0600)
	journalPath, _ := installUnfinishedV2MutationGateFixture(t, root, environment, filepath.Join(root, "runtime"), func(script string) string {
		// Retain the fixture's valid completed-response envelope and add native readiness.
		script = strings.Replace(script, `"status":"ready"`, `"status":"operator-action-required"`, 1)
		script = strings.Replace(script, `"reachedGoal":true`, `"reachedGoal":false`, 1)
		script = strings.Replace(script, `"code":"ready"`, `"code":"dependency-unavailable"`, 1)
		script = strings.Replace(script, `"message":"verified"`, `"message":"native collision","retry":"repair address"`, 1)
		return strings.Replace(script, `"outcome":`, `"blockers":[{"code":"dependency-unavailable","resource":"activation.fixture.native-target","message":"native collision","retry":"repair address"}],"outcome":`, 1)
	})
	journal, err := releasetransition.ParseJournal(mustReadConfigFixture(t, journalPath))
	if err != nil {
		t.Fatal(err)
	}
	journal.Checkpoint = releasetransition.JournalComplete
	history, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, journalPath, history, 0600)
	links, err := releasetransition.NewRuntimeLinkStore(filepath.Join(root, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := links.Activate(journal.Releases); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	options := cli.options
	options.Environment, options.Stdout, options.Stderr = environment, &output, &output
	options.Arguments = []string{"-Y", "named", "config", "set", "INCUS_PROJECT", "subyard-named", "--scope", "yard", "--yes"}
	program, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	code := program.Run(context.Background())
	repaired, err := cli.loadContext("named")
	if code != 0 || err != nil || repaired.Context.IncusProject != "subyard-named" {
		t.Fatalf("native repair blocked: exit=%d err=%v %s", code, err, &output)
	}
	if !bytes.Equal(history, mustReadConfigFixture(t, journalPath)) || len(native.ConfigUpdates) != 0 || len(native.ExecCalls) != 0 {
		t.Fatal("address repair changed release history or native state")
	}
}

func TestNativeTargetAddressRepairIsBounded(t *testing.T) {
	inspection := &releasetransition.Inspection{Blockers: []releasetransition.Blocker{{Code: releasetransition.CodeDependencyUnavailable, Resource: "activation.fixture.native-target"}}}
	for _, test := range []struct {
		arguments []string
		allowed   bool
	}{
		{[]string{"set", "INCUS_PROJECT", "separate", "--scope", "yard", "--yes"}, true},
		{[]string{"unset", "YARD_INSTANCE_NAME", "--scope", "host"}, true},
		{[]string{"set", "SRV_VOLUME", "separate", "--scope", "yard"}, true},
		{[]string{"set", "INCUS_PROJECT", "separate", "--scope", "shared"}, false},
		{[]string{"set", "INCUS_PROJECT", "separate", "--scope", "yard", "--git"}, false},
		{[]string{"set", "SSH_PORT", "2224", "--scope", "yard"}, false},
	} {
		if got := nativeTargetAddressRepair("@config", test.arguments, inspection); got != test.allowed {
			t.Fatalf("address admission %v = %v", test.arguments, got)
		}
	}
	inspection.Blockers = append(inspection.Blockers, releasetransition.Blocker{Code: releasetransition.CodeDependencyUnavailable, Resource: "activation.other"})
	if nativeTargetAddressRepair("@config", []string{"set", "INCUS_PROJECT", "separate", "--scope", "yard"}, inspection) {
		t.Fatal("unrelated blocker admitted")
	}

}

func TestNativeTargetSurfacesRefuseBeforeMutation(t *testing.T) {
	for _, tc := range []struct{ name, command, flags, owner, want string }{
		{"init", "init", "", "", "share native target"},
		{"init-foreign", "init", "", "foreign", "belongs to yard foreign"},
		{"fresh", "init", "", "", "share native target"},
		{"identity-owned", "init", "", "named", "share native target"},
		{"identity-unmarked", "init", "", "unmarked", "share native target"},
		{"status", "config", "status", "named", "yard scope for default"},
		{"status-empty", "config", "status --all-local", "", "no local yards selected"},
		{"status-broken", "config", "status --all-local", "", "broken"},
		{"apply", "config", "apply", "", "share native target"},
		{"apply-refresh", "config", "apply", "", "share native target"},
		{"teardown", "teardown", "", "named", "yard scope for default"},
		{"teardown-full", "teardown", "--full", "", "share native target"},
		{"hard-cleanup", "cleanup", "--hard", "", "share native target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, native, home := nativeTargetFixture(t)
			host := "INCUS_PROJECT=custom\nYARD_INSTANCE_NAME=box\nSRV_VOLUME=shared-data\n"
			if strings.HasPrefix(tc.name, "status-") {
				host = "ACCESS_KIND=remote\nREMOTE_DEST=owner.example\nREMOTE_YARD=default\n"
			}
			writeEmptyNamedFile(t, filepath.Join(home, "config.env"), host, 0600)
			if tc.name == "status-broken" {
				writeEmptyNamedFile(t, filepath.Join(home, "yards/broken/config.env"), "invalid assignment\n", 0600)
			}
			if tc.owner != "" {
				instance := nativeInstance(tc.owner)
				instance.Name, instance.Status = "box", "Stopped"
				if tc.owner == "unmarked" {
					delete(instance.Config, "user.subyard.name")
					delete(instance.LocalConfig, "user.subyard.name")
				}
				native.Instances["custom/box"] = instance
			}
			loaded, err := cli.loadReleaseTransitionContext("named", home, nil)
			if err != nil {
				t.Fatal(err)
			}
			platform, applier := newInitPlatformFixture(), &recordingConfigApplier{}
			cli.options.InitPlatform, cli.options.Config = platform, applier
			path := filepath.Join(home, "yards/named/config.env")
			before := mustReadConfigFixture(t, path)
			guard := &collisionReadGuard{Incus: native}
			if tc.name == "hard-cleanup" {
				cli.options.Incus = guard
			}
			var output bytes.Buffer
			options := cli.options
			options.Stderr = &output
			yard := "named"
			if tc.name == "fresh" {
				yard = "fresh"
			}
			options.Arguments = append([]string{"-Y", yard, tc.command}, strings.Fields(tc.flags)...)
			options.Arguments = append(options.Arguments, "--yes")
			program, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			if code := program.Run(context.Background()); code == 0 {
				t.Fatal("refusal accepted")
			}
			if tc.name == "apply-refresh" {
				targets := []configTarget{{Name: "named", Loaded: loaded}}
				err := cli.refreshConfigApply(context.Background(), &configApplyExecution{approved: []configTargetAssessment{{Target: targets[0]}}, selector: func() ([]configTarget, error) { return targets, nil }})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("refresh refusal: %v", err)
				}
				output.WriteString(err.Error())
			}
			if !strings.Contains(output.String(), tc.want) {
				t.Fatalf("missing %q: %s", tc.want, &output)
			}
			if tc.name == "status-broken" && strings.Contains(output.String(), "no local yards selected") {
				t.Fatal(&output)
			}
			if !strings.HasPrefix(tc.name, "status-") && !strings.Contains(output.String(), "share /srv volume") {
				t.Fatal("lost volume refusal", &output)
			}
			if (tc.name == "status" || tc.name == "teardown") && strings.Contains(output.String(), "yard scope for named") {
				t.Fatal("detached known owner", &output)
			}
			if tc.name == "teardown" {
				execution, _ := prepareTeardownExecution(nil)
				other := loaded.Context
				other.YardName = "other"
				execution.Yards = []uninstall.RecoveryYard{{Context: other}, {Context: loaded.Context}}
				execution.selectYard(loaded)
				if execution.activeYard != &execution.Yards[1] {
					t.Fatal("selected native alias instead of yard")
				}
			}
			for _, absent := range []string{hardCleanupPath(loaded.Context), filepath.Join(home, "yards/fresh/config.env")} {
				if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refusal published %s: %v", absent, err)
				}
			}
			if !bytes.Equal(before, mustReadConfigFixture(t, path)) || string(mustReadConfigFixture(t, filepath.Join(home, "config.env"))) != host || len(platform.applied) != 0 || len(platform.preflightFresh) != 0 || len(applier.yards) != 0 || len(native.ExecCalls) != 0 || len(native.ConfigUpdates) != 0 || guard.reads != 0 {
				t.Fatal("refusal mutated state or entered guest")
			}
		})
	}
}
