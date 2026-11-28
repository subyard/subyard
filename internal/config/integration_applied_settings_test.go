package config

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestAppliedIntegrationSettingsKeepOnlyValidatedInitInputs(t *testing.T) {
	catalog := Catalog{}
	for _, suffix := range []string{"COMMAND", "CHECK", "HEALTH", "PROJECTS_CHANGED", "DEPENDS", "PERSIST", "PROVISION"} {
		name := "AGENT_sample_" + suffix
		if !catalog.IntegrationInitSetting(name) {
			t.Fatalf("init setting omitted: %s", name)
		}
	}
	values := map[string]string{"AGENT_sample_COMMAND": "sample", "AGENT_sample_CHECK": "sample-check", "HOST_CODEX_AGENTS_MD": "/tmp/retired", "DEV_USER": "dev", "AGENT_sample_CONFIG": "/tmp/config", "UNRELATED": "private", "SECRET_TOKEN": "private"}
	settings := catalog.IntegrationInitSettings(values)
	if len(settings) != 3 {
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
	if projected["AGENT_sample_COMMAND"] != "old-command" || projected["DEV_USER"] != "dev" || projected["UNRELATED"] != "retained" || projected["CODING_TOOL_GIT_APPROVAL"] != "prompt" {
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
	desired["CODING_TOOL_GIT_APPROVAL"] = "off"
	legacy := catalog.WithAppliedIntegrationInitSettings(desired, nil)
	if legacy["CODING_TOOL_GIT_APPROVAL"] != "prompt" {
		t.Fatal("legacy inventory without a snapshot lost its installed prompt mode")
	}
	if legacy["AGENT_sample_PROJECTS_CHANGED"] != "new-hook" {
		t.Fatal("legacy unknown baseline was treated as a complete snapshot")
	}
}

// Default-mode snapshots stay readable by readers that predate the setting;
// such readers refuse any unknown field, so only off is recorded.
func TestAppliedIntegrationSettingsPreserveGitApprovalEncoding(t *testing.T) {
	catalog := Catalog{}
	for _, value := range []string{"", "prompt"} {
		settings := catalog.IntegrationInitSettings(map[string]string{"DEV_USER": "dev", "CODING_TOOL_GIT_APPROVAL": value})
		if len(settings) != 1 || settings["DEV_USER"] != "dev" {
			t.Fatalf("implicit applied mode %q entered the snapshot: %#v", value, settings)
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

// The default prompt stays out of the snapshot, so older runtimes can read it;
// yes and no are recorded. A snapshot without the setting, also one from before
// it, reads as prompt whatever the desired mode.
func TestAppliedIntegrationSettingsRecordOnlyNonDefaultSudo(t *testing.T) {
	catalog := Catalog{}
	for _, value := range []string{"prompt", ""} {
		if settings := catalog.IntegrationInitSettings(map[string]string{"DEV_SUDO": value}); len(settings) != 0 {
			t.Fatalf("default sudo mode %q entered the snapshot: %#v", value, settings)
		}
	}
	recorded := map[string]map[string]string{}
	for _, value := range []string{"yes", "no"} {
		settings := catalog.IntegrationInitSettings(map[string]string{"DEV_SUDO": value})
		if len(settings) != 1 || settings["DEV_SUDO"] != value || catalog.ValidateIntegrationInitSettings(settings) != nil {
			t.Fatalf("sudo mode %q was not recorded: %#v", value, settings)
		}
		recorded[value] = settings
	}
	for _, test := range []struct {
		desired string
		applied map[string]string
		want    string
	}{
		{"yes", map[string]string{}, "prompt"}, {"no", map[string]string{}, "prompt"}, {"prompt", map[string]string{}, "prompt"},
		{"", map[string]string{}, "prompt"}, {"yes", nil, "prompt"}, {"no", nil, "prompt"},
		{"prompt", recorded["yes"], "yes"}, {"no", recorded["yes"], "yes"}, {"prompt", recorded["no"], "no"}, {"yes", recorded["no"], "no"},
	} {
		if got := catalog.WithAppliedIntegrationInitSettings(map[string]string{"DEV_SUDO": test.desired}, test.applied)["DEV_SUDO"]; got != test.want {
			t.Fatalf("desired %q with snapshot %#v projected as %q, want %q", test.desired, test.applied, got, test.want)
		}
	}
	for _, value := range []string{"true", "off", "2"} {
		if catalog.ValidateIntegrationInitSettings(map[string]string{"DEV_SUDO": value}) == nil {
			t.Fatalf("installed snapshot accepted unsupported DEV_SUDO=%s", value)
		}
	}
}

// A snapshot can record names this release does not read, such as a newer
// release's settings or process variables; they never reach the environment.
func TestAppliedIntegrationSettingsProjectOnlyNamesThisReleaseReads(t *testing.T) {
	catalog := Catalog{}
	applied := map[string]string{"DEV_USER": "dev", "FUTURE_SETTING": "x", "PATH": "/injected"}
	environment := catalog.WithAppliedIntegrationInitSettings(map[string]string{"PATH": "/usr/bin"}, applied)
	if environment["DEV_USER"] != "dev" || environment["PATH"] != "/usr/bin" {
		t.Fatalf("projected environment = %#v", environment)
	}
	if _, present := environment["FUTURE_SETTING"]; present {
		t.Fatal("unknown snapshot name reached the environment")
	}
	if readable := catalog.ReadableAppliedIntegrationInitSettings(applied); len(readable) != 1 || readable["DEV_USER"] != "dev" {
		t.Fatalf("readable = %#v", readable)
	}
	if catalog.ReadableAppliedIntegrationInitSettings(nil) != nil {
		t.Fatal("absent baseline became a snapshot")
	}
	if readable := catalog.ReadableAppliedIntegrationInitSettings(map[string]string{"FUTURE_SETTING": "x"}); readable == nil || len(readable) != 0 {
		t.Fatalf("snapshot of unknown names = %#v", readable)
	}
	if catalog.ValidateIntegrationInitSettings(map[string]string{"FUTURE_SETTING": "x"}) == nil {
		t.Fatal("the writer's closed domain accepted an unknown field")
	}
}

func TestAppliedIntegrationsOfAnotherReleaseAreNotInstalledEvidence(t *testing.T) {
	catalog, err := LoadCatalog(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	desired := map[string]string{"AGENT_custom_COMMAND": "custom"}
	// futuretool was shipped by another release, bare has no record, and
	// removed is a custom integration no longer in the desired values.
	applied := map[string]string{"AGENT_futuretool_COMMAND": "future", "AGENT_futuretool_PROVISION": "/srv/subyard/releases/r2/config/agents/futuretool/provision.sh",
		"AGENT_futuretool_extra_COMMAND": "kept", "AGENT_custom_COMMAND": "custom", "AGENT_removed_COMMAND": "removed", "AGENT_removed_CONFIG": "/home/dev/.config/subyard/overrides/host/agents/removed/config.toml"}
	selected, settings := catalog.ReadableAppliedIntegrations(desired, []string{"custom", "futuretool", "bare", "removed", "bad/name"}, applied)
	if !slices.Equal(selected, []string{"custom", "removed", "bad/name"}) || len(settings) != 4 || settings["AGENT_custom_COMMAND"] != "custom" ||
		settings["AGENT_removed_CONFIG"] == "" || settings["AGENT_futuretool_extra_COMMAND"] != "kept" || len(applied) != 6 {
		t.Fatalf("selected %q settings %#v applied %#v", selected, settings, applied)
	}
	if selected, settings := catalog.ReadableAppliedIntegrations(desired, []string{"futuretool"}, nil); len(selected) != 0 || settings != nil {
		t.Fatalf("nil snapshot became %#v", settings)
	}
}
