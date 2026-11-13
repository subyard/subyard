package config

import "testing"

func TestAppliedIntegrationSettingsKeepOnlyValidatedInitInputs(t *testing.T) {
	catalog := Catalog{}
	for _, suffix := range []string{"COMMAND", "CHECK", "HEALTH", "PROJECTS_CHANGED", "DEPENDS", "PERSIST", "PROVISION"} {
		name := "AGENT_sample_" + suffix
		if !catalog.IntegrationInitSetting(name) {
			t.Fatalf("init setting omitted: %s", name)
		}
	}
	values := map[string]string{"AGENT_sample_COMMAND": "sample", "AGENT_sample_CHECK": "sample-check", "HOST_CODEX_AGENTS_MD": "/tmp/rules", "DEV_USER": "dev", "AGENT_sample_CONFIG": "/tmp/config", "UNRELATED": "private", "SECRET_TOKEN": "private"}
	settings := catalog.IntegrationInitSettings(values)
	if len(settings) != 4 {
		t.Fatalf("unexpected input count: %d", len(settings))
	}
	if err := catalog.ValidateIntegrationInitSettings(settings); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"UNRELATED", "SECRET_TOKEN", "AGENT_sample_CONFIG", "CODING_TOOL_INTEGRATIONS"} {
		if err := catalog.ValidateIntegrationInitSettings(map[string]string{name: "private"}); err == nil {
			t.Fatalf("unsupported applied field accepted: %s", name)
		}
	}
}

func TestAppliedIntegrationSettingsValidateHostOnlyPackageInput(t *testing.T) {
	catalog := Catalog{profiles: map[string]SettingDefinition{"SYNTHETIC_INPUT": {Kind: SettingScalar, Type: SettingName, Application: SettingYardInit, Owner: "integration:synthetic", Scopes: []SettingScope{ScopeHost}}}}
	if err := catalog.ValidateIntegrationInitSettings(map[string]string{"SYNTHETIC_INPUT": "value"}); err != nil {
		t.Fatal(err)
	}
}

func TestAppliedIntegrationSettingsPreserveAbsentInputs(t *testing.T) {
	catalog := Catalog{}
	before := map[string]string{"AGENT_sample_COMMAND": "old-command", "DEV_USER": "dev"}
	applied := catalog.IntegrationInitSettings(before)
	desired := map[string]string{"AGENT_sample_COMMAND": "new-command", "DEV_USER": "other", "AGENT_sample_PROJECTS_CHANGED": "new-hook", "AGENT_sample_HEALTH": "new-health", "UNRELATED": "retained"}
	projected := catalog.WithAppliedIntegrationInitSettings(desired, applied)
	if projected["AGENT_sample_COMMAND"] != "old-command" || projected["DEV_USER"] != "dev" || projected["UNRELATED"] != "retained" {
		t.Fatal("applied domain projection changed unrelated state or lost applied values")
	}
	for _, name := range []string{"AGENT_sample_PROJECTS_CHANGED", "AGENT_sample_HEALTH"} {
		if _, ok := projected[name]; ok {
			t.Fatalf("new desired field leaked into applied inspection: %s", name)
		}
	}
	if desired["DEV_USER"] != "other" || desired["AGENT_sample_PROJECTS_CHANGED"] != "new-hook" {
		t.Fatal("applied inspection changed desired inputs")
	}
	legacy := catalog.WithAppliedIntegrationInitSettings(desired, nil)
	if legacy["AGENT_sample_PROJECTS_CHANGED"] != "new-hook" {
		t.Fatal("legacy unknown baseline was treated as a complete snapshot")
	}
}

// Default-mode snapshots stay readable by readers that predate the setting;
// such readers refuse any unknown field, so only off is recorded.
func TestAppliedIntegrationSettingsRecordOnlyNonDefaultGitApproval(t *testing.T) {
	catalog := Catalog{}
	for _, value := range []string{"", "prompt"} {
		settings := catalog.IntegrationInitSettings(map[string]string{"DEV_USER": "dev", "CODING_TOOL_GIT_APPROVAL": value})
		if len(settings) != 1 || settings["DEV_USER"] != "dev" {
			t.Fatalf("default mode %q entered the snapshot: %#v", value, settings)
		}
	}
	settings := catalog.IntegrationInitSettings(map[string]string{"CODING_TOOL_GIT_APPROVAL": "off"})
	if settings["CODING_TOOL_GIT_APPROVAL"] != "off" || catalog.ValidateIntegrationInitSettings(settings) != nil {
		t.Fatalf("off mode was not recorded: %#v", settings)
	}
	if catalog.ValidateIntegrationInitSettings(map[string]string{"CODING_TOOL_FUTURE_SETTING": "off"}) == nil {
		t.Fatal("closed applied-settings domain accepted an unknown field")
	}
}
