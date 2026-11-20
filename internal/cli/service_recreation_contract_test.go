package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

// releaseRuntimeFixture is a runtime root whose active release is previous-release.
func releaseRuntimeFixture(t *testing.T) string {
	t.Helper()
	root := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "releases/previous-release"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/previous-release", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	return root
}

// The release transition entry point binds recreation onto the native broker owner.
func TestReleaseTransitionReconcilersBindServiceRecreation(t *testing.T) {
	program, _, _, _, _ := serviceRecreationFixtureFor(t, "test-vm-broker")
	home := environmentValue(program.options.Environment, "SUBYARD_CONFIG_HOME")
	runtimeRoot := releaseRuntimeFixture(t)
	for _, test := range []struct {
		target    releasetransition.ReleaseID
		direction releasetransition.Direction
		bound     bool
	}{
		{"next-release", releasetransition.DirectionActivateTarget, true},
		{"previous-release", releasetransition.DirectionActivateTarget, true},
		{"next-release", releasetransition.DirectionActivatePrevious, false},
	} {
		request := releasetransition.ProcessRequest{ConfigHome: home, RuntimeRoot: runtimeRoot, Target: test.target, Direction: test.direction}
		var broker *activationStageReconciler
		for _, owner := range program.releaseTransitionReconcilers(request) {
			if stage, ok := owner.(*activationStageReconciler); ok && stage.ID() == "test-vm-broker" {
				broker = stage
			}
		}
		if broker == nil || (broker.recreation != nil) != test.bound {
			t.Fatalf("%+v: broker owner %+v", test, broker)
		}
	}
}

// A refused recreation keeps the previous engine on the next same-release gate.
func TestRefusedRecreationStaysRefusedOnSameReleaseGate(t *testing.T) {
	_, _, selection, stage, calls := serviceRecreationFixture(t)
	foreign := selection.record
	foreign.Owner = strings.Repeat("f", 32)
	if err := selection.registry.Remove(selection.record, selection.publication); err != nil {
		t.Fatal(err)
	}
	if err := selection.registry.Save(foreign); err != nil {
		t.Fatal(err)
	}
	for _, releases := range []releasetransition.ReleasePair{{From: "previous-release", Target: "next-release"}, {From: "next-release", Target: "next-release"}} {
		links := releasetransition.ReleaseLinks{Active: releases.Target}
		observed, err := stage.Observe(context.Background(), releases, links)
		if err != nil || !observed.Converged || len(observed.Warnings) != 1 || !strings.Contains(observed.Warnings[0], "keeps its previous engine") {
			t.Fatalf("%+v: refused observation %+v %v", releases, observed, err)
		}
		if err := stage.Reconcile(context.Background(), links); err != nil || len(calls.calls) != 0 || len(calls.platform.applied) != 0 {
			t.Fatalf("%+v: refused runtime changed: %v %v %v", releases, err, calls.calls, calls.platform.applied)
		}
	}
}

// legacyTestYardFixture registers a pre-v0.11 test yard beside a default yard.
func legacyTestYardFixture(t *testing.T) (*CLI, string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
	writeConfigCommandFile(t, filepath.Join(home, "yards", legacyTestYardName, "config.env"), "SSH_PORT=2240\n")
	program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: &testkit.Incus{}})
	if err != nil {
		t.Fatal(err)
	}
	return program, home
}

func TestLegacyTestYardIsLeftToItsRecreation(t *testing.T) {
	program, home := legacyTestYardFixture(t)
	request := releasetransition.ProcessRequest{ConfigHome: home}
	t.Run("readiness-warning", func(t *testing.T) {
		applicability, err := program.brokerActivationWithFiles(request, nil).inspectApplicability(context.Background())
		if err != nil || !slices.Contains(applicability.warnings, legacyTestYardWarning) {
			t.Fatalf("legacy warning: %+v %v", applicability, err)
		}
	})
	t.Run("config-targets", func(t *testing.T) {
		legacy, err := program.loadContext(legacyTestYardName)
		if err != nil {
			t.Fatal(err)
		}
		if targets, err := program.releaseConfigTargets(context.Background(), legacy, false); err != nil || len(targets) != 0 {
			t.Fatalf("release work selected the legacy yard: %+v %v", targets, err)
		}
		if targets, err := program.localConfigTargets(legacy, false); err != nil || len(targets) != 1 {
			t.Fatalf("ordinary config work lost the legacy yard: %+v %v", targets, err)
		}
		all, err := program.releaseConfigTargets(context.Background(), legacy, true)
		if err != nil || slices.ContainsFunc(all, func(target configTarget) bool { return target.Name == legacyTestYardName }) || len(all) == 0 {
			t.Fatalf("all-local release work: %+v %v", all, err)
		}
	})
	t.Run("registered-yard-migration", func(t *testing.T) {
		owner, ok := program.registeredYardMigration(request, false, legacyTestYardName).(*migration.RegisteredYardMigration)
		if !ok {
			t.Fatal("registered yard migration owner changed type")
		}
		if targets, err := owner.LoadTargets(context.Background(), nil); err != nil || len(targets) != 0 {
			t.Fatalf("legacy yard became a migration target: %+v %v", targets, err)
		}
	})
}

// Prospective materialized activation leaves a staged legacy yard to its recreation.
func TestPreparedActivationSkipsLegacyTestYard(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	reconciler := fixture.reconciler(false).materializedConfigActivationReconciler
	reconciler.allLocal = true
	releases := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
	plan := func(names []string, snapshots map[string]config.PersistentFileSnapshot) releasetransition.V2SourceActivationPlan {
		t.Helper()
		view := preparedActivationSnapshotFixture{home: fixture.configHome, names: names, snapshots: snapshots}
		projected, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, releasetransition.V2SourceActivationView{Settings: view})
		if err != nil {
			t.Fatal(err)
		}
		return projected
	}
	names, err := config.YardNames(fixture.configHome)
	if err != nil {
		t.Fatal(err)
	}
	without := plan(names, map[string]config.PersistentFileSnapshot{})
	legacy := filepath.Join(fixture.configHome, "yards", legacyTestYardName, "config.env")
	with := plan(append(slices.Clone(names), legacyTestYardName), map[string]config.PersistentFileSnapshot{legacy: {Exists: true, Content: []byte("SSH_PORT=2240\n")}})
	if with.Desired != without.Desired {
		t.Fatal("prospective activation planned the legacy yard")
	}
}
