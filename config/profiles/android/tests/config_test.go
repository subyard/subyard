package tests

import (
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
)

func TestAndroidShippedSettings(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	catalog, err := config.LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"ADB_CONSOLE_EMULATOR_PORT": "5554", "ADB_EMULATOR_PORT": "5555", "ADB_PROXY_PORT": "15555"} {
		definition, ok := catalog.LookupSetting(name)
		if !ok || !definition.HasDefault || definition.Default != want || definition.Application != config.SettingNextCommand {
			t.Fatalf("%s = %#v", name, definition)
		}
	}
}
