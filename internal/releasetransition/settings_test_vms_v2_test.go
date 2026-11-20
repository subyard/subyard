package releasetransition

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func settingsV2Fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	configHome := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(configHome, "yards"), 0o700); err != nil {
		t.Fatal(err)
	}
	for relative, content := range files {
		path := filepath.Join(configHome, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(content), 0600)
	}
	return configHome
}
