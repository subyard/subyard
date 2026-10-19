package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

func preparedInstalledFixture(t *testing.T) Runtime {
	t.Helper()
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := testkit.TempDir(t)
	paths := []string{"scripts/lib-power.sh", "scripts/install-key-tools.sh", "scripts/install-keys-auto-sync.sh", "config/systemd/subyard-keys-sync.service.in", "config/systemd/subyard-keys-sync.timer.in"}
	for _, leaf := range installedRuntimeHelpers {
		paths = append(paths, "scripts/lib/"+leaf)
	}
	for _, path := range paths {
		payload, err := os.ReadFile(filepath.Join(source, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, path), payload, 0o755)
	}
	home := testkit.TempDir(t)
	if err := os.MkdirAll(filepath.Join(home, "tools/bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	return Runtime{RepositoryRoot: root, Yard: domain.Context{Paths: domain.RuntimePaths{DataHome: home, ConfigHome: home, OperatorHome: home}}, Environment: []string{"SUBYARD_KEYS_TOOLS_DIR=" + filepath.Join(home, "tools")}}
}

func TestPreparedInstalledRecipeRestoresOriginalBytesAndRefusesSourceDrift(t *testing.T) {
	runtime := preparedInstalledFixture(t)
	p, err := runtime.PrepareInstalledRuntime(context.Background(), "key-tools-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.directory != "" {
		t.Fatal("prepare created execution tree")
	}
	original := p.Recipe()
	restored, err := runtime.BindInstalledRuntime(original)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.Binding() != p.Binding() {
		t.Fatal("restored different approved recipe")
	}
	testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "scripts/install-key-tools.sh"), []byte("#!/bin/sh\nexit 99\n"), 0o755)
	if _, err := runtime.BindInstalledRuntime(original); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("changed source rebound: %v", err)
	}
	if err := runtime.ApplyPreparedInstalledRuntime(context.Background(), restored, "prepared", testRuntimeOldDigest, testRuntimeNewDigest); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("changed source executed: %v", err)
	}
	if restored.directory != "" {
		t.Fatal("stale source created execution tree")
	}
	if err := p.materialize(); err != nil {
		t.Fatal(err)
	}
	cached, err := os.ReadFile(filepath.Join(p.directory, "scripts/install-key-tools.sh"))
	if err != nil || string(cached) != string(p.files["scripts/install-key-tools.sh"]) {
		t.Fatal("payload recaptured", err)
	}
}

func TestPreparedInstalledRecipeRejectsUnrecognizedExecutableAndLeafDrift(t *testing.T) {
	runtime := preparedInstalledFixture(t)
	p, err := runtime.PrepareInstalledRuntime(context.Background(), "key-tools-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var recipe installedRecipe
	if err := json.Unmarshal(p.Recipe(), &recipe); err != nil {
		t.Fatal(err)
	}
	recipe.Files[0].Path = "scripts/arbitrary-command.sh"
	raw, _ := json.Marshal(recipe)
	if _, err := runtime.BindInstalledRuntime(raw); err == nil {
		t.Fatal("unrecognized executable admitted")
	}
	if err := os.Remove(filepath.Join(runtime.Yard.Paths.DataHome, "tools/bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.BindInstalledRuntime(p.Recipe()); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("changed leaf set rebound: %v", err)
	}
}

func TestPreparedInstalledExecutionUsesApprovedBytesAfterSourceGuard(t *testing.T) {
	runtime := preparedInstalledFixture(t)
	home := runtime.Yard.Paths.DataHome
	marker := filepath.Join(home, "effect")
	swap := filepath.Join(home, "source-after-guard")
	original := filepath.Join(runtime.RepositoryRoot, "scripts/install-key-tools.sh")
	payload := []byte("#!/bin/sh\nset -eu\ncase \"$1\" in --check) [ -f \"$EFFECT\" ] && exit 0; if [ -f \"$SWAP\" ]; then cp \"$SWAP\" \"$ORIGINAL\"; rm \"$SWAP\"; fi; exit 1;; esac\nprintf approved >\"$EFFECT\"\n")
	testkit.WriteFile(t, original, payload, 0o755)
	testkit.WriteFile(t, swap, []byte("#!/bin/sh\nprintf unauthorized >\"$EFFECT\"\n"), 0o755)
	runtime.Environment = append(runtime.Environment, "EFFECT="+marker, "SWAP="+swap, "ORIGINAL="+original)
	p, err := runtime.PrepareInstalledRuntime(context.Background(), "key-tools-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Capture the original native baseline without triggering this fixture's
	// source swap; the first execution reobservation performs it after source CAS.
	if err := os.Rename(swap, swap+".held"); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.ObserveInstalledRuntime(context.Background(), "key-tools-runtime")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swap+".held", swap); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyPreparedInstalledRuntime(context.Background(), p, "prepared", before.Actual, before.Desired); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(marker)
	if err != nil || string(actual) != "approved" {
		t.Fatal("unapproved source executed", err)
	}
	if _, err := os.Stat(p.directory); !os.IsNotExist(err) {
		t.Fatal("execution tree survived drained command", err)
	}
}
