package config

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
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
		"AGENT_<name>_CHECK", "AGENT_<name>_HEALTH", "AGENT_<name>_PROJECTS_CHANGED", "AGENT_<name>_DEPENDS",
		"AGENT_<name>_PERSIST",
	} {
		if !strings.Contains(string(content), pattern) {
			t.Errorf("public settings example is missing dynamic pattern %s", pattern)
		}
	}
}

func TestOperationCatalogProfileSettingsAreIsolatedAndTyped(t *testing.T) {
	metadata, err := profile.Decode("fixture", []byte(`{"schema_version":1,"settings":[{"name":"FIXTURE_PORT","type":"port","scopes":["shipped","command"],"application":"next-command","minimum":1,"maximum":65535,"default":"2345","syncable":true,"host_listener":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := BuildCatalog([]profile.Metadata{metadata}, nil)
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
	metadata, err := profile.Decode("fixture", []byte(`{"schema_version":1,"settings":[{"name":"SSH_PORT","type":"port","scopes":["command"],"application":"next-command","minimum":1,"maximum":65535}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildCatalog([]profile.Metadata{metadata}, nil); err == nil {
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

	values, err := EvaluateAssignments(strings.NewReader(assignment+"\n"), examplePath, nil)
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

func TestPowerArtifactSettingsRemainHostLocal(t *testing.T) {
	for _, name := range []string{"SUBYARD_POWER_RECONCILER_PATH", "SUBYARD_POWER_UNIT_PATH"} {
		for _, scope := range []SettingScope{ScopeShipped, ScopeHost, ScopeCommand} {
			if err := ValidateSetting(scope, name, "/opt/subyard/power", false); err != nil {
				t.Errorf("%s rejected %s scope: %v", name, scope, err)
			}
		}
		for _, scope := range []SettingScope{ScopeShared, ScopeYard, SettingScope("unknown")} {
			if err := ValidateSetting(scope, name, "/opt/subyard/power", false); err == nil {
				t.Errorf("%s accepted %s scope", name, scope)
			}
		}
		if err := ValidateSetting(ScopeHost, name, "relative/power", false); err == nil {
			t.Errorf("%s accepted a relative host path", name)
		}
		definition, err := ValidateSettingName(ScopeHost, name, false)
		if err != nil {
			t.Fatal(err)
		}
		if definition.Type != SettingAbsolutePath || definition.Application != SettingYardInit || definition.Syncable {
			t.Fatalf("%s lost its local typed contract: %+v", name, definition)
		}
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
	values := environment{"SSH_PORT": "2345", "ENVIRONMENT_PROFILES": "fixture", "FIXTURE_PORT": "2345"}
	if err := normalizeDerivedPorts(values, tracker); err == nil {
		t.Fatal("profile host listener collision was ignored")
	}
}

func TestCatalogExportsDoNotChangeValidationOrDeclarations(t *testing.T) {
	metadata, err := profile.Decode("fixture", []byte(`{"schema_version":1,"disabled":true,"disabled_when":{"OFF":"1"},"settings":[{"name":"FIXTURE_MODE","type":"string","enum":["safe","fast"],"scopes":["shipped","command"],"application":"next-command","default":"safe"},{"name":"FIXTURE_PORT","type":"port","scopes":["shipped","command"],"application":"next-command","minimum":1,"maximum":65535,"default_from":{"setting":"SSH_PORT","offset":1000}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	integration, err := profile.DecodeIntegration("sample", []byte(`{"schema_version":1,"lifecycle":{"handler":"lifecycle.sh","source_files":["source"],"instance_config_keys":["user.subyard.sample"],"instance_devices":["sample"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := BuildCatalog([]profile.Metadata{metadata}, []profile.IntegrationMetadata{integration})
	if err != nil {
		t.Fatal(err)
	}
	profiles := settings.Profiles()
	if !profiles[0].Disabled {
		t.Fatal("catalog lost disabled profile policy")
	}
	profiles[0].Disabled = false
	profiles[0].DisabledWhen["OFF"] = "changed"
	profiles[0].Settings[0].Enum[0] = "unsafe"
	profiles[0].Settings[0].Scopes[0] = "host"
	*profiles[0].Settings[0].Default = "unsafe"
	profiles[0].Settings[1].DefaultFrom.Setting = "FOREIGN"
	integrations := settings.Integrations()
	integrations[0].Lifecycle.SourceFiles[0] = "foreign"
	for _, definition := range settings.SettingCatalog() {
		if len(definition.Scopes) > 0 {
			definition.Scopes[0] = ScopeHost
		}
		if len(definition.Enum) > 0 {
			definition.Enum[0] = "unsafe"
		}
		if definition.DefaultFrom != nil {
			definition.DefaultFrom.Setting = "FOREIGN"
		}
	}
	field, _ := settings.LookupSetting("FIXTURE_MODE")
	field.Enum[0] = "unsafe"
	field.Scopes[0] = ScopeHost
	port, _ := settings.LookupSetting("FIXTURE_PORT")
	port.DefaultFrom.Setting = "FOREIGN"
	if err := settings.ValidateSetting(ScopeCommand, "FIXTURE_MODE", "unsafe", false); err == nil {
		t.Fatal("export changed enum validation")
	}
	if err := settings.ValidateSetting(ScopeHost, "FIXTURE_MODE", "safe", false); err == nil {
		t.Fatal("export changed scope validation")
	}
	again := settings.Profiles()[0]
	if !again.Disabled || again.DisabledWhen["OFF"] != "1" || *again.Settings[0].Default != "safe" || again.Settings[1].DefaultFrom.Setting != "SSH_PORT" || settings.Integrations()[0].Lifecycle.SourceFiles[0] != "source" {
		t.Fatal("export changed retained declaration")
	}
	if port, _ := settings.LookupSetting("FIXTURE_PORT"); port.DefaultFrom.Setting != "SSH_PORT" {
		t.Fatal("export changed derived default")
	}
	// Core catalog values have the same ownership contract.
	core, _ := LookupSetting("SSH_PORT")
	core.Scopes[0] = ScopeCommand
	againCore, _ := LookupSetting("SSH_PORT")
	if againCore.Scopes[0] == ScopeCommand {
		t.Fatal("core catalog aliases caller")
	}
}
