package testkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileSetsExactModeOnCreateAndReplace(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640, 0o644, 0o700, 0o755} {
		path := filepath.Join(t.TempDir(), "fixture")
		for _, content := range []string{"created", "replaced"} {
			WriteFile(t, path, []byte(content), mode)
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("fixture mode: %v, %v; want %o", info, err, mode)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != content {
				t.Fatalf("fixture content: %q, %v", data, err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestTempDirIsPrivate(t *testing.T) {
	path := TempDir(t)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("private fixture directory: %v, %v", info, err)
	}
}

func TestUseHostTempDirPreservesRunRootAndRefusesWorkspacePlacement(t *testing.T) {
	UseHostTempDir(t)
	scope := TempDir(t)
	for _, scenario := range []struct {
		name, scope, expected string
	}{
		{"scoped", scope, scope},
		{"direct Go test", "", "/tmp"},
		{"outside temporary storage", "/", "/tmp"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("SUBYARD_RUN_TEMP", scenario.scope)
			t.Setenv("TMPDIR", filepath.Join(scope, "checkout"))
			t.Setenv("GOTMPDIR", filepath.Join(scope, "checkout"))
			UseHostTempDir(t)
			for _, name := range []string{"TMPDIR", "GOTMPDIR"} {
				if actual := os.Getenv(name); actual != scenario.expected {
					t.Fatalf("%s=%q, want %q", name, actual, scenario.expected)
				}
			}
			if scenario.name == "scoped" {
				if path := TempDir(t); !strings.HasPrefix(path, scope+string(filepath.Separator)) {
					t.Fatalf("fixture escaped the run root: %s", path)
				}
			}
		})
	}
}
