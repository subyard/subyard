package tests

import (
	"os"
	"path/filepath"
	"slices"
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

// The emulator renderer is a typed per-yard field. The profile passes its
// effective value through; its own fallback matches the shipped default.
func TestAndroidEmulatorGPUIsATypedPerYardField(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	catalog, err := config.LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := catalog.LookupSetting("EMULATOR_GPU")
	if !ok || !definition.HasDefault || definition.Default != "software-gles" || definition.Application != config.SettingYardInit ||
		strings.Join(definition.Enum, " ") != "software-gles software host" || definition.Owner != "profile:android" {
		t.Fatalf("EMULATOR_GPU = %#v", definition)
	}
	scopes := []string{}
	for _, scope := range definition.Scopes {
		scopes = append(scopes, string(scope))
	}
	if !slices.Contains(scopes, "yard") || !slices.Contains(scopes, "host") {
		t.Fatalf("EMULATOR_GPU scopes = %v", scopes)
	}
	path := filepath.Join("..", "profile.conf")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "software-gles", "software", "host"} {
		base := map[string]string{}
		if value != "" {
			base["EMULATOR_GPU"] = value
		}
		values, err := config.EvaluateAssignments(strings.NewReader(string(source)), path, base)
		if err != nil {
			t.Fatal(err)
		}
		want := value
		if want == "" {
			want = definition.Default
		}
		if values["EMULATOR_GPU"] != want || values["YARD_DEVICES"] != "kvm" {
			t.Fatalf("EMULATOR_GPU=%q evaluates to %q devices=%q", value, values["EMULATOR_GPU"], values["YARD_DEVICES"])
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
