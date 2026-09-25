package testkit

import (
	"os"
	"testing"
)

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
