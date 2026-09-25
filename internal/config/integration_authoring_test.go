package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestYardIntegrationWriteMigratesFullRegistration(t *testing.T) {
	for _, source := range []string{"flat", "private"} {
		t.Run(source, func(t *testing.T) {
			root := testkit.TempDir(t)
			configHome := filepath.Join(root, "state")
			configDir := filepath.Join(root, "config")
			path := filepath.Join(configHome, "yards/demo.env")
			if source == "private" {
				path = filepath.Join(root, "private/yards/demo.env")
			}
			original := "# operator settings\nSSH_PORT=2244\nHOST_LINKS=\nAGENTS=paseo\n"
			writeFixture(t, path, original)
			if err := os.MkdirAll(configHome, 0o700); err != nil {
				t.Fatal(err)
			}
			loaded := Loaded{Context: domain.Context{YardName: "demo", Paths: domain.RuntimePaths{ConfigHome: configHome, ConfigDir: configDir}}}
			plan, err := PlanYardIntegrationWrite(loaded, []string{"paseo", "pi"})
			if err != nil {
				t.Fatal(err)
			}
			if !plan.Changed() || plan.SourcePath != path {
				t.Fatalf("plan=%#v", plan)
			}
			if _, err := os.Stat(plan.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("plan created target: %v", err)
			}
			if err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(plan.Path)
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "# operator settings\nSSH_PORT=2244\nHOST_LINKS=\nCODING_TOOL_INTEGRATIONS='paseo pi'\n" {
				t.Fatalf("registration lost settings: %q", content)
			}
			preserved := path
			if source == "flat" {
				preserved = filepath.Join(configHome, "recovery/yard-registrations/demo.env")
			}
			bytes, err := os.ReadFile(preserved)
			if err != nil || string(bytes) != original {
				t.Fatalf("source lost: %q, %v", bytes, err)
			}
			again, err := PlanYardIntegrationWrite(loaded, []string{"paseo", "pi"})
			if err != nil || again.Changed() {
				t.Fatalf("repeat=%#v, %v", again, err)
			}
		})
	}
}

func TestYardIntegrationWriteRejectsStaleSource(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "yards/demo.env")
	writeFixture(t, path, "SSH_PORT=2244\n")
	loaded := Loaded{Context: domain.Context{YardName: "demo", Paths: domain.RuntimePaths{ConfigHome: root, ConfigDir: filepath.Join(root, "config")}}}
	plan, err := PlanYardIntegrationWrite(loaded, []string{})
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, "SSH_PORT=2245\n")
	if err := plan.Apply(); !errors.Is(err, ErrPersistentTargetStale) {
		t.Fatalf("stale source accepted: %v", err)
	}
	if _, err := os.Stat(plan.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale adoption wrote target: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), "2245") {
		t.Fatalf("source changed: %q %v", content, err)
	}
}

func TestYardIntegrationWriteRejectsSameBytesReplacementAndPreservesNoOp(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "yards/default/config.env")
	writeFixture(t, path, "CODING_TOOL_INTEGRATIONS='codex'\n")
	loaded := Loaded{Context: domain.Context{YardName: "default", Paths: domain.RuntimePaths{ConfigHome: root}}}
	plan, err := PlanYardIntegrationWrite(loaded, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("no-op replaced settings: %v", err)
	}
	pending := path + ".other"
	writeFixture(t, pending, "CODING_TOOL_INTEGRATIONS='codex'\n")
	if err := os.Rename(pending, path); err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); !errors.Is(err, ErrPersistentTargetStale) {
		t.Fatalf("same-bytes inode replacement accepted: %v", err)
	}
}

func TestYardIntegrationWriteRejectsNewSourceAuthority(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "yards/default/config.env")
			if existing {
				writeFixture(t, path, "CODING_TOOL_INTEGRATIONS='codex'\n")
			}
			loaded := Loaded{Context: domain.Context{YardName: "default", Paths: domain.RuntimePaths{ConfigHome: root}}}
			plan, err := PlanYardIntegrationWrite(loaded, []string{})
			if err != nil {
				t.Fatal(err)
			}
			writeFixture(t, filepath.Join(root, SourceRecordRelativePath), "{}\n")
			if err := plan.Apply(); err == nil || !strings.Contains(err.Error(), "source-managed") {
				t.Fatalf("new source accepted: %v", err)
			}
			current, err := readOptionalIntegrationSnapshot(root, path)
			if err != nil || !samePersistentFileSnapshotExact(current, plan.Before) {
				t.Fatalf("source-managed target changed: %v", err)
			}
		})
	}
}

func TestPersistentCASGuardHoldsLockBeforeMutation(t *testing.T) {
	root := testkit.TempDir(t)
	path := filepath.Join(root, "config.env")
	writeFixture(t, path, "CODING_TOOL_INTEGRATIONS='codex'\n")
	before, err := ReadPersistentFileSnapshot(root, path)
	if err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("authority changed")
	called := false
	guard := func() error {
		called = true
		file, err := os.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			t.Fatalf("guard did not hold exclusive root lock: %v", err)
		}
		return rejected
	}
	if err := compareAndSwapPersistentFileGuarded(root, path, before, []byte("CODING_TOOL_INTEGRATIONS=''\n"), guard, nil); !errors.Is(err, rejected) || !called {
		t.Fatalf("guard=%v called=%v", err, called)
	}
	after, err := ReadPersistentFileSnapshot(root, path)
	if err != nil || !samePersistentFileSnapshotExact(before, after) {
		t.Fatalf("guard changed settings: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".config.env.release-transition.pending")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guard created pending file: %v", err)
	}
}
