package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLegacyGitOwnershipBlocksSettingsButAllowsRuntimeLocks(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, ".sync"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, ".sync", "manifest.json"),
		[]byte(`{"schemaVersion":1,"files":[{"path":"config.env"}]}`), 0o600)
	setting := filepath.Join(root, "config.env")
	if err := CreatePersistentFile(root, setting, []byte("SSH_PORT=2222\n")); err == nil || !strings.Contains(err.Error(), "migrate Git settings") {
		t.Fatalf("legacy settings write: %v", err)
	}
	if _, err := os.Stat(setting); !os.IsNotExist(err) {
		t.Fatalf("refused setting was created: %v", err)
	}
	lock := filepath.Join(root, ".locks", "integrations", "default.lock")
	if err := CreatePersistentFile(root, lock, nil); err != nil {
		t.Fatalf("legacy Git ownership blocked an unrelated runtime lock: %v", err)
	}
}
