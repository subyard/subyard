package testkit

import (
	"os"
	"path/filepath"
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
