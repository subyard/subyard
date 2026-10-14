package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPreparedActivationRejectsWrongBoundOwnerRegistrationAndOverrides(t *testing.T) {
	for _, field := range []string{"registration", "overrides"} {
		t.Run(field, func(t *testing.T) {
			fixture := newSourceScopeFixture(t)
			registration := filepath.Join(fixture.configHome, "yards", "e2e-yard", "config.env")
			if err := os.MkdirAll(filepath.Dir(registration), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, registration, []byte("YARD_TEMPLATE=test-vms\n"), 0600)
			final := []byte("YARD_TEMPLATE='test-vms'\n")
			view := preparedActivationSnapshotFixture{home: fixture.configHome, names: []string{"e2e-yard"}, snapshots: map[string]config.PersistentFileSnapshot{registration: {Exists: true, Content: final}}}
			owner := newInterruptedRenameOwner(t, fixture.root, fixture.configHome)
			before, err := owner.Prepare(context.Background(), view)
			if err != nil {
				t.Fatal(err)
			}
			if before.Registration != releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(final))) {
				t.Fatal("owner did not bind final prepared settings")
			}
			if field == "registration" {
				before.Registration = releasetransition.Fingerprint(strings.Repeat("1", 64))
			} else {
				before.Overrides = releasetransition.Fingerprint(strings.Repeat("2", 64))
			}
			draft, err := fixture.program.prepareSourceActivationDraft(fixture.configHome, releasetransition.V2SourceActivationView{Settings: view, OwnerRegistration: &before})
			if draft != nil {
				t.Fatal("invalid owner binding returned a draft")
			}
			if err == nil {
				t.Fatal("wrong prepared owner binding was accepted")
			}
			if _, err := os.Stat(owner.current); !os.IsNotExist(err) {
				t.Fatal("invalid owner binding published registration", err)
			}
			if len(fixture.boundary.applied) != 0 {
				t.Fatal("invalid owner binding reached activation writes")
			}
			journal, err := fixture.store.ReadCurrentJournal()
			if err != nil || journal.Exists {
				t.Fatal("invalid owner binding published checkpoint", err)
			}
		})
	}
}

type preparedActivationSnapshotFixture struct {
	home      string
	snapshots map[string]config.PersistentFileSnapshot
	names     []string
}

type unreadPreparedActivationFixture struct {
	preparedActivationSnapshotFixture
	reads int
}

func (view *unreadPreparedActivationFixture) ReadPreparedInput(path string) (config.PersistentFileSnapshot, bool, error) {
	if _, exists := view.snapshots[path]; exists {
		view.reads++
		return config.PersistentFileSnapshot{}, true, errors.New("unrelated captured payload must not be read")
	}
	return config.PersistentFileSnapshot{}, false, nil
}

func TestPreparedActivationDoesNotReadUnrelatedLargeInventory(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	testkit.WriteFile(t, filepath.Join(fixture.root, "config", "projects-changed.sh"), []byte("#!/bin/sh\n"), 0755)
	fixture.program.options.InitPlatform = nil
	fixture.program.options.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	fixture.program.options.Executor = nil
	view := &unreadPreparedActivationFixture{preparedActivationSnapshotFixture: preparedActivationSnapshotFixture{home: fixture.configHome, snapshots: map[string]config.PersistentFileSnapshot{}}}
	payload := []byte(strings.Repeat("unrelated", 1024))
	for index := 0; index < 5000; index++ {
		path := filepath.Join(fixture.configHome, "secrets", "legacy", "qa-pool", fmt.Sprintf("archive-%d", index))
		view.snapshots[path] = config.PersistentFileSnapshot{Exists: true, Content: payload}
	}
	if len(view.snapshots)*len(payload) <= 32<<20 {
		t.Fatal("fixture does not exceed the removed aggregate bound")
	}
	reconciler := fixture.reconciler(false).materializedConfigActivationReconciler
	releases := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
	native, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	projected, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, releasetransition.V2SourceActivationView{Settings: view})
	if err != nil || projected.Desired != native.Desired || view.reads != 0 {
		t.Fatalf("unused inventory changed assessment or was read: projected=%s native=%s reads=%d err=%v", projected.Desired, native.Desired, view.reads, err)
	}
}

func TestPreparedActivationPreservesUncapturedNativeInstructionSymlinkAndLargeAsset(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	testkit.WriteFile(t, filepath.Join(fixture.root, "config", "projects-changed.sh"), []byte("#!/bin/sh\n"), 0755)
	fixture.program.options.InitPlatform = nil
	fixture.program.options.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	fixture.program.options.Executor = nil
	target := filepath.Join(fixture.configHome, "instructions-large.md")
	link := filepath.Join(fixture.configHome, "instructions.md")
	payload := []byte(strings.Repeat("Native instructions.\n", 450000))
	if len(payload) <= 8<<20 {
		t.Fatal("fixture does not exceed persistent settings size")
	}
	testkit.WriteFile(t, target, payload, 0600)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(fixture.configHome, "config.env")
	settings, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, host, append(settings, []byte("HOST_CLAUDE_MD='"+link+"'\n")...), 0600)
	reconciler := fixture.reconciler(false).materializedConfigActivationReconciler
	releases := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
	native, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	view := preparedActivationSnapshotFixture{home: fixture.configHome}
	projected, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, releasetransition.V2SourceActivationView{Settings: view})
	if err != nil || projected.Desired != native.Desired {
		t.Fatalf("native asset reader policy changed: projected=%s native=%s err=%v", projected.Desired, native.Desired, err)
	}
	draft, err := fixture.program.prepareSourceActivationDraft(fixture.configHome, releasetransition.V2SourceActivationView{Settings: view})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := draft.readAsset(config.MaterializedAsset{Source: link}); err == nil {
		t.Fatal("strict uncaptured asset accepted a symlink")
	}
	if actual, err := draft.readAsset(config.MaterializedAsset{Source: target}); err != nil || len(actual) != len(payload) {
		t.Fatal("large native regular asset was narrowed", err)
	}
}

func (view preparedActivationSnapshotFixture) ListYards() ([]string, error) {
	return slices.Clone(view.names), nil
}
func (view preparedActivationSnapshotFixture) PreparedInputPaths() ([]string, error) {
	var paths []string
	for path := range view.snapshots {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths, nil
}
func (view preparedActivationSnapshotFixture) ReadSnapshot(path string) (config.PersistentFileSnapshot, error) {
	if snapshot, ok := view.snapshots[path]; ok {
		return snapshot, nil
	}
	return config.ReadPersistentFileSnapshot(view.home, path)
}

func (view preparedActivationSnapshotFixture) ReadPreparedInput(path string) (config.PersistentFileSnapshot, bool, error) {
	snapshot, exists := view.snapshots[path]
	return snapshot, exists, nil
}

func TestPreparedMaterializedActivationMatchesNativeImportedAssetsAndNewYard(t *testing.T) {
	fixture := newSourceScopeFixture(t)
	testkit.WriteFile(t, filepath.Join(fixture.root, "config", "projects-changed.sh"), []byte("#!/bin/sh\n"), 0755)
	fixture.program.options.InitPlatform = nil
	fixture.program.options.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
	fixture.program.options.Executor = nil
	reconciler := fixture.reconciler(false).materializedConfigActivationReconciler
	releases := releasetransition.ReleasePair{From: "release-b", Target: "release-b"}
	before, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	hostAsset := filepath.Join(fixture.configHome, "overrides", "host", "agents", "claude", "settings.json")
	newAsset := filepath.Join(fixture.configHome, "yards", "test-yard", "overrides", "agents", "claude", "settings.json")
	registration := filepath.Join(fixture.configHome, "yards", "test-yard", "config.env")
	view := preparedActivationSnapshotFixture{home: fixture.configHome, names: []string{"named", "test-yard"}, snapshots: map[string]config.PersistentFileSnapshot{
		hostAsset:    {Exists: true, Content: []byte(`{"env":{"SCOPE":"default-imported"}}`)},
		newAsset:     {Exists: true, Content: []byte(`{"env":{"SCOPE":"canonical-imported"}}`)},
		registration: {Exists: true, Content: []byte("SSH_PORT=2244\nAGENT_claude_CONFIG='" + newAsset + "'\n")},
	}}
	projected, err := reconciler.PrepareSourceActivation(context.Background(), releases, releasetransition.ReleaseLinks{}, releasetransition.V2SourceActivationView{Settings: view})
	if err != nil {
		t.Fatal(err)
	}
	if projected.Desired == before.Desired {
		t.Fatal("projection did not bind imported assets and new yard")
	}
	if _, err := os.Stat(registration); !os.IsNotExist(err) {
		t.Fatal("prospective assessment published registration", err)
	}
	if got, err := os.ReadFile(hostAsset); err != nil || string(got) == string(view.snapshots[hostAsset].Content) {
		t.Fatal("prospective assessment wrote imported bytes", err)
	}
	for path, snapshot := range view.snapshots {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, snapshot.Content, 0600)
	}
	after, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if projected.Desired != after.Desired {
		t.Fatalf("projected %s != native %s", projected.Desired, after.Desired)
	}
	if len(fixture.boundary.applied) != 0 {
		t.Fatal("desired comparison performed activation writes")
	}
	profile := fixture.program.profileActivationReconciler(fixture.request, "sample-runtime").(*profileRuntimeActivationReconciler)
	stableBefore, err := profile.desired(releases)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, hostAsset, []byte(`{"env":{"SCOPE":"unexpected-drift"}}`), 0600)
	drift, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{})
	if err != nil {
		t.Fatal(err)
	}
	if drift.Desired == projected.Desired {
		t.Fatal("unexpected asset drift retained projected authority")
	}
	fixture.program.options.Incus.(*testkit.Incus).Instances["subyard-test-yard/yard-test-yard"] = ports.InstanceInfo{Project: "subyard-test-yard", Name: "yard-test-yard", Status: "Running"}
	if _, err := reconciler.Observe(context.Background(), releases, releasetransition.ReleaseLinks{}); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatal("unapproved backend availability was accepted", err)
	}
	if len(fixture.boundary.applied) != 0 {
		t.Fatal("backend drift reached config activation writes")
	}
	journal, err := fixture.store.ReadCurrentJournal()
	if err != nil || journal.Exists {
		t.Fatal("adapter assessment published a journal", err)
	}
	stableAfter, err := profile.desired(releases)
	if err != nil || stableAfter != stableBefore {
		t.Fatal("static profile Desired changed", err)
	}
}
