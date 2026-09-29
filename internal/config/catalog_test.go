package config

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
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

func TestOperationCatalogProfileSettingsAreIsolatedAndTyped(t *testing.T) {
	root := t.TempDir()
	writeCatalogProfile(t, root, "fixture", `{"schema_version":1,"settings":[{"name":"FIXTURE_PORT","type":"port","scopes":["shipped","command"],"application":"next-command","minimum":1,"maximum":65535,"default":"2345","syncable":true,"host_listener":true}]}`)
	catalog, err := LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := catalog.LookupSetting("FIXTURE_PORT")
	if !ok || definition.Default != "2345" || !definition.HasDefault || !definition.HostListener || definition.Application != SettingNextCommand {
		t.Fatalf("profile definition = %#v", definition)
	}
	if err := catalog.ValidateSetting(ScopeCommand, "FIXTURE_PORT", "65536", false); err == nil {
		t.Fatal("invalid profile port accepted")
	}
	if _, ok := (Catalog{}).LookupSetting("FIXTURE_PORT"); ok {
		t.Fatal("operation-local profile setting leaked into zero catalog")
	}
}

func TestOperationCatalogRejectsCoreSettingCollision(t *testing.T) {
	root := t.TempDir()
	writeCatalogProfile(t, root, "fixture", `{"schema_version":1,"settings":[{"name":"SSH_PORT","type":"port","scopes":["command"],"application":"next-command","minimum":1,"maximum":65535}]}`)
	if _, err := LoadCatalog(root); err == nil {
		t.Fatal("profile setting collision with core catalog accepted")
	}
}

func writeCatalogProfile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, "config", "profiles", name, "profile.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(content), 0o600)
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
	if got := values["ENVIRONMENT_PROFILES"]; got == "" {
		t.Fatal("copyable profile example is empty")
	} else if err := ValidateSetting(ScopeYard, "ENVIRONMENT_PROFILES", got, false); err != nil {
		t.Fatalf("copyable profile example is invalid: %v", err)
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

func TestAgentCleanupSettingUsesProvisionHookContract(t *testing.T) {
	const name = "AGENT_example_CLEANUP"
	for _, scope := range []SettingScope{ScopeShipped, ScopeHost, ScopeYard, ScopeCommand} {
		if err := ValidateSetting(scope, name, "/opt/subyard/example-cleanup", false); err != nil {
			t.Errorf("ValidateSetting(%s, %s) returned %v", scope, name, err)
		}
	}
	if err := ValidateSetting(ScopeShared, name, "/opt/subyard/example-cleanup", false); err == nil {
		t.Error("shared cleanup hook override was accepted")
	}
	definition, err := ValidateSettingName(ScopeHost, name, false)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Kind != SettingFile || definition.Type != SettingRegularFilePath ||
		definition.Application != SettingYardInit || definition.Syncable {
		t.Fatalf("unexpected cleanup definition: %+v", definition)
	}
	if !sourceValuedAgentSetting(name) {
		t.Fatal("cleanup hook was not classified as a source-valued agent setting")
	}
}

func TestProfileDefaultsLayersAndListenerCollision(t *testing.T) {
	root := syntheticResourceRoot(t)
	writeCatalogProfile(t, root, "fixture", `{"schema_version":1,"settings":[{"name":"FIXTURE_PORT","type":"port","scopes":["shipped","host","yard","command"],"application":"next-command","minimum":1,"maximum":65535,"default":"2345","syncable":true,"host_listener":true}]}`)
	home := testkit.TempDir(t)
	configHome := filepath.Join(home, ".config", "subyard")
	options := LoadOptions{RepositoryRoot: root, OperatorHome: home, DisablePrivate: true, Environment: map[string]string{"SUBYARD_OPERATOR_HOME": home}}
	check := func(want, scope string) {
		t.Helper()
		loaded, err := Load(options)
		if err != nil {
			t.Fatal(err)
		}
		trace := loaded.Settings["FIXTURE_PORT"]
		if trace.EffectiveValue != want {
			t.Fatalf("profile value = %q, want %q", trace.EffectiveValue, want)
		}
		found := false
		for _, item := range trace.Resolutions {
			found = found || item.Status == "effective" && item.Scope == scope
		}
		if !found {
			t.Fatalf("missing winning scope %s: %#v", scope, trace)
		}
	}
	check("2345", "default")
	writeFixture(t, filepath.Join(configHome, "config.env"), "FIXTURE_PORT=2346\n")
	check("2346", "host")
	writeFixture(t, filepath.Join(configHome, "yards", "default", "config.env"), "FIXTURE_PORT=2347\n")
	check("2347", "yard")
	options.Environment["FIXTURE_PORT"] = "2348"
	check("2348", "command")
	settings, err := LoadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	tracker := newSettingTracker()
	tracker.catalog = settings
	values := environment{"CODING_TOOL_INTEGRATIONS": "aiobserver", "AI_OBSERVER_HOST_PORT": "2345", "FIXTURE_PORT": "2345"}
	if err := normalizeAIObserverPort(values, tracker, 2222); err == nil {
		t.Fatal("profile host listener collision was ignored")
	}
}
