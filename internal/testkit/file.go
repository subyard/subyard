package testkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UseHostTempDir keeps Git fixtures outside the checkout. A top-level test run keeps
// its private /tmp root, so killed fixtures remain sweepable; direct Go tests use /tmp.
func UseHostTempDir(t testing.TB) {
	t.Helper()
	base := "/tmp"
	if scope, err := filepath.EvalSymlinks(os.Getenv("SUBYARD_RUN_TEMP")); err == nil && strings.HasPrefix(scope, "/tmp/") {
		if info, err := os.Stat(scope); err == nil && info.IsDir() {
			base = scope
		}
	}
	t.Setenv("TMPDIR", base)
	// Go 1.26 also uses GOTMPDIR for testing.T.TempDir.
	t.Setenv("GOTMPDIR", base)
}

// TempDir creates a private fixture root independent of the caller's umask.
func TempDir(t testing.TB) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// WriteFile gives a fixture its exact mode, including when replacing an existing file.
func WriteFile(t testing.TB, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
