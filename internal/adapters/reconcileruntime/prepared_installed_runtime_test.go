package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func preparedGuestInstalledFixture(t *testing.T) Runtime {
	t.Helper()
	runtime := preparedInstalledFixture(t)
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"scripts/refresh-installed-guest-runtime.sh", "scripts/prepare-preview-route.sh", "config/preview/subyard-preview", "config/agents/ccusage/provision.sh"} {
		payload, err := os.ReadFile(filepath.Join(source, relative))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(runtime.RepositoryRoot, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, payload, 0755)
	}
	runtime.LogicalRepositoryRoot = runtime.RepositoryRoot
	runtime.Environment = []string{"ALLOWS_CODING_TOOLS=true", "CCUSAGE_PROVISION=" + filepath.Join(runtime.RepositoryRoot, "config/agents/ccusage/provision.sh")}
	return runtime
}
func retainedGuestRuntime(t *testing.T, runtime Runtime) Runtime {
	t.Helper()
	root, err := os.Open(runtime.RepositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	runtime.RepositoryRoot = fmt.Sprintf("/proc/self/fd/%d", root.Fd())
	runtime.Environment = []string{"ALLOWS_CODING_TOOLS=true", "CCUSAGE_PROVISION=" + fmt.Sprintf("/proc/%d/fd/%d/config/agents/ccusage/provision.sh", os.Getpid(), root.Fd())}
	return runtime
}
func TestPreparedGuestBuiltinHookUsesRetainedRepositoryAnchor(t *testing.T) {
	runtime := preparedGuestInstalledFixture(t)
	original, err := runtime.PrepareInstalledRuntime(context.Background(), "core-guest-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	retained := retainedGuestRuntime(t, runtime)
	current, err := retained.PrepareInstalledRuntime(context.Background(), "core-guest-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if !bytes.Equal(original.Recipe(), current.Recipe()) {
		t.Fatal("retained spelling changed approved recipe")
	}
	for _, fresh := range []Runtime{retained, retainedGuestRuntime(t, runtime)} {
		bound, err := fresh.BindInstalledRuntime(original.Recipe())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bound.Recipe(), original.Recipe()) {
			t.Fatal("fresh descriptor changed original recipe")
		}
		bound.Close()
	}
}
func TestPreparedGuestHookRefusesUnapprovedAliasesAndSourceDrift(t *testing.T) {
	for _, change := range []string{"bytes", "replacement", "mode", "mtime", "absent", "custom-bytes", "other-root", "custom-parent-link", "builtin-parent-link", "builtin-leaf-link", "foreign-pid", "other-fd"} {
		t.Run(change, func(t *testing.T) {
			runtime := preparedGuestInstalledFixture(t)
			if change == "custom-bytes" {
				path := filepath.Join(testkit.TempDir(t), "hook.sh")
				testkit.WriteFile(t, path, []byte("#!/bin/sh\nexit 0\n"), 0755)
				runtime.Environment = []string{"ALLOWS_CODING_TOOLS=true", "CCUSAGE_PROVISION=" + path}
			}
			original, err := runtime.PrepareInstalledRuntime(context.Background(), "core-guest-runtime")
			if err != nil {
				t.Fatal(err)
			}
			defer original.Close()
			path := runtime.environmentValue("CCUSAGE_PROVISION")
			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "bytes", "custom-bytes":
				testkit.WriteFile(t, path, append(payload, '\n'), 0755)
			case "replacement":
				testkit.WriteFile(t, path+".new", payload, 0755)
				if err := os.Rename(path+".new", path); err != nil {
					t.Fatal(err)
				}
			case "mtime":
				if err := os.Chtimes(path, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
					t.Fatal(err)
				}
			case "absent":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "other-root":
				other := preparedGuestInstalledFixture(t)
				other.LogicalRepositoryRoot = runtime.LogicalRepositoryRoot
				other.Yard = runtime.Yard
				runtime = retainedGuestRuntime(t, other)
				if runtime.configRuntimeBinding() != original.runtime {
					t.Fatal("root identity fixture changed command inputs")
				}
			case "custom-parent-link":
				dir := testkit.TempDir(t)
				if err := os.Symlink(filepath.Dir(path), filepath.Join(dir, "link")); err != nil {
					t.Fatal(err)
				}
				runtime.Environment = []string{"ALLOWS_CODING_TOOLS=true", "CCUSAGE_PROVISION=" + filepath.Join(dir, "link/provision.sh")}
			case "builtin-parent-link":
				if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+".held"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(path)+".held", filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
			case "builtin-leaf-link":
				if err := os.Rename(path, path+".held"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".held", path); err != nil {
					t.Fatal(err)
				}
			case "foreign-pid":
				runtime.Environment = []string{"ALLOWS_CODING_TOOLS=true", "CCUSAGE_PROVISION=" + fmt.Sprintf("/proc/1/fd/0/config/agents/ccusage/provision.sh")}
			case "other-fd":
				fd, err := os.Open(runtime.RepositoryRoot)
				if err != nil {
					t.Fatal(err)
				}
				defer fd.Close()
				runtime.Environment = []string{"ALLOWS_CODING_TOOLS=true", "CCUSAGE_PROVISION=" + fmt.Sprintf("/proc/%d/fd/%d/config/agents/ccusage/provision.sh", os.Getpid(), fd.Fd())}
			}
			if bound, err := runtime.BindInstalledRuntime(original.Recipe()); err == nil {
				bound.Close()
				t.Fatal("changed hook/source rebound")
			}
			if change == "custom-parent-link" || change == "builtin-parent-link" || change == "builtin-leaf-link" || change == "foreign-pid" || change == "other-fd" {
				if fresh, err := runtime.PrepareInstalledRuntime(context.Background(), "core-guest-runtime"); err == nil {
					fresh.Close()
					t.Fatal("unapproved hook prepared")
				}
			}
			if err := runtime.ApplyPreparedInstalledRuntime(context.Background(), original, "prepared", testRuntimeOldDigest, testRuntimeNewDigest); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("changed hook/source reached execution admission")
			}
			if original.directory != "" {
				t.Fatal("refusal materialized execution tree")
			}
		})
	}
}
