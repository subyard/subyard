package tests

import (
	"os"
	"path/filepath"
	"strings"
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

func TestAndroidJDKPathsFollowConfiguredMajor(t *testing.T) {
	path := filepath.Join("..", "profile.conf")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	values, err := config.EvaluateAssignments(strings.NewReader(strings.ReplaceAll(string(source), "JDK_VERSION=17", "JDK_VERSION=21")), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if values["JAVA_HOME"] != "/opt/jdk-21" {
		t.Fatalf("JAVA_HOME = %q", values["JAVA_HOME"])
	}
	if !strings.Contains(":"+values["PATH"]+":", ":/opt/jdk-21/bin:") {
		t.Fatalf("PATH = %q", values["PATH"])
	}
	if !strings.Contains(" "+values["ENV_MOUNTS"]+" ", " /opt/jdk-21:/opt/jdk-21:ro ") {
		t.Fatalf("ENV_MOUNTS = %q", values["ENV_MOUNTS"])
	}
}
