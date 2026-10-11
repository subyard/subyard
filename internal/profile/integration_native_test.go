package profile

import (
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"testing"
)

func TestIntegrationNativeSourceCopyAndExactBinding(t *testing.T) {
	root := testkit.TempDir(t)
	packageRoot := filepath.Join(root, "config", "agents", "sample")
	if err := os.MkdirAll(packageRoot, 0755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "go.mod"), []byte("module source-copy\n"), 0644)
	testkit.WriteFile(t, filepath.Join(packageRoot, "integration.json"), []byte(`{"schema_version":1,"native":[{"package":"cmd/tool","path":"bin/tool"}]}`), 0644)
	native := filepath.Join(root, ".build", "agents", "sample", "bin", "tool")
	if err := os.MkdirAll(filepath.Dir(native), 0755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, native, []byte("native-v1"), 0755)
	definitions, err := LoadIntegrations(root)
	if err != nil {
		t.Fatal(err)
	}
	definition := definitions[0]
	path, err := definition.NativeExecutablePath("bin/tool")
	if err != nil || path != native {
		t.Fatalf("plain source resolution = %q, %v", path, err)
	}
	before, err := definition.SourceBinding()
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, native, []byte("native-v2"), 0755)
	after, err := definition.SourceBinding()
	if err != nil || before == after {
		t.Fatal("native bytes not bound")
	}
	if err := os.Remove(native); err != nil {
		t.Fatal(err)
	}
	if _, err := definition.SourceBinding(); err == nil {
		t.Fatal("missing helper got hash fallback")
	}
	testkit.WriteFile(t, native, []byte("development-v3"), 0755)
	installed := filepath.Join(packageRoot, "bin", "tool")
	if err := os.MkdirAll(filepath.Dir(installed), 0755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, installed, []byte("sealed-native"), 0755)
	if path, err := definition.NativeExecutablePath("bin/tool"); err != nil || path != installed {
		t.Fatalf("installed resolution = %q, %v", path, err)
	}
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(native, installed); err != nil {
		t.Fatal(err)
	}
	if _, err := definition.NativeExecutablePath("bin/tool"); err == nil {
		t.Fatal("symlink hidden by development fallback")
	}
	if _, err := definition.NativeExecutablePath("bin/undeclared"); err == nil {
		t.Fatal("undeclared sidecar resolved")
	}
}

func TestIntegrationNativeValidation(t *testing.T) {
	for _, declaration := range []string{
		`{"schema_version":1,"native":[{"package":"../tool","path":"bin/tool"}]}`,
		`{"schema_version":1,"native":[{"package":"cmd/tool","path":"../tool"}]}`,
		`{"schema_version":1,"native":[{"package":"cmd/tool","path":"bin/tool"},{"package":"cmd/other","path":"bin/tool"}]}`,
	} {
		root := testkit.TempDir(t)
		if err := os.MkdirAll(filepath.Join(root, "config", "agents", "sample"), 0755); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "config", "agents", "sample", "integration.json"), []byte(declaration), 0644)
		if _, err := LoadIntegrations(root); err == nil {
			t.Fatal("invalid native declaration accepted")
		}
	}
}
