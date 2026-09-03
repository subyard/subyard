package config

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPublicSettingsExampleCoversStaticCatalog(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	examplePath := filepath.Join(root, "config", "settings.env.example")
	content, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}

	assignments := map[string]struct{}{}
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		name, _, found := strings.Cut(line, "=")
		if found && ValidVariable(name) {
			assignments[name] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for _, definition := range SettingCatalog() {
		if _, ok := assignments[definition.Name]; !ok {
			t.Errorf("public settings example is missing %s", definition.Name)
		}
	}
	for _, pattern := range []string{
		"AGENT_<name>_CONFIG", "AGENT_<name>_RULES", "AGENT_<name>_CONFIG_DEST",
		"AGENT_<name>_RULES_DEST", "AGENT_<name>_PROVISION", "AGENT_<name>_COMMAND",
		"AGENT_<name>_CHECK", "AGENT_<name>_PROJECTS_CHANGED", "AGENT_<name>_DEPENDS",
		"AGENT_<name>_PERSIST",
	} {
		if !strings.Contains(string(content), pattern) {
			t.Errorf("public settings example is missing dynamic pattern %s", pattern)
		}
	}
}

func TestPublicEnvironmentProfilesExampleIsCopyable(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	examplePath := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "config", "settings.env.example"))
	content, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}

	var assignment string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ENVIRONMENT_PROFILES=") {
			assignment = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			break
		}
	}
	if assignment == "" {
		t.Fatal("public settings example is missing ENVIRONMENT_PROFILES")
	}

	path := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(path, []byte(assignment+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := ReadAssignments(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := values["ENVIRONMENT_PROFILES"]; got != "openclaw" {
		t.Fatalf("copyable ENVIRONMENT_PROFILES = %q, want %q", got, "openclaw")
	}
}

func TestCodexReleasePinsAreTypedYardInitSettings(t *testing.T) {
	definitions := make(map[string]SettingDefinition)
	for _, definition := range SettingCatalog() {
		definitions[definition.Name] = definition
	}
	for name, valueType := range map[string]SettingValueType{
		"CODEX_VERSION":      SettingVersion,
		"CODEX_SHA256_AMD64": SettingSHA256,
		"CODEX_SHA256_ARM64": SettingSHA256,
	} {
		definition, ok := definitions[name]
		if !ok {
			t.Errorf("catalog is missing %s", name)
			continue
		}
		if definition.Type != valueType {
			t.Errorf("%s type = %s, want %s", name, definition.Type, valueType)
		}
		if definition.Application != SettingYardInit || !definition.Syncable {
			t.Errorf("%s must be a syncable yard-init setting", name)
		}
	}
}

func TestAgentDependencySettingAcceptsDeclaredScopes(t *testing.T) {
	const name = "AGENT_paseo_DEPENDS"
	for _, scope := range []SettingScope{ScopeShipped, ScopeHost, ScopeYard, ScopeCommand} {
		if err := ValidateSetting(scope, name, "codex opencode", false); err != nil {
			t.Errorf("ValidateSetting(%s, %s) returned %v", scope, name, err)
		}
	}
	if err := ValidateSetting(ScopeShared, name, "codex", false); err == nil {
		t.Error("shared dependency override was accepted")
	}
	if err := ValidateSetting(ScopeHost, name, "../codex", false); err == nil {
		t.Error("unsafe dependency ID was accepted")
	}

	definition, err := ValidateSettingName(ScopeHost, name, false)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Type != SettingNameList || definition.Application != SettingYardInit || definition.Syncable {
		t.Fatalf("unexpected dependency definition: %+v", definition)
	}
}
