package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// unknownSettingsFixture loads the default yard from the repository catalog
// with one settings file; FUTURE_* names are synthetic and unknown to it.
func unknownSettingsFixture(t *testing.T, files map[string]string) (Loaded, string, error) {
	t.Helper()
	home := filepath.Join(testkit.TempDir(t), "home")
	configHome := filepath.Join(home, ".config", "subyard")
	for relative, content := range files {
		writeFixture(t, filepath.Join(configHome, relative), content)
	}
	loaded, err := Load(LoadOptions{
		RepositoryRoot: filepath.Clean(filepath.Join("..", "..")), OperatorHome: home,
		Environment: map[string]string{
			"SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": configHome,
			"SUBYARD_HOME": filepath.Join(home, ".subyard"),
		},
	})
	return loaded, configHome, err
}

func TestUnknownSettingsAreIgnoredInEveryOperatorLayer(t *testing.T) {
	for _, test := range []struct{ relative, scope string }{
		{"overrides/shared/config.env", "shared"},
		{"config.env", "host"},
		{"yards/default/config.env", "yard"},
		{"yards/default.env", "yard"},
		{".sync/settings/overrides/shared/config.env", "shared"},
		{".sync/settings/config.env", "host"},
		{".sync/settings/yards/default/config.env", "yard"},
	} {
		t.Run(test.relative, func(t *testing.T) {
			content := "# newer release\nFUTURE_SETTING='future value'\nLD_PRELOAD=/tmp/inject.so\nFUTURE_EMPTY=\nGIT_USER_NAME=Example\n"
			loaded, configHome, err := unknownSettingsFixture(t, map[string]string{test.relative: content})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(configHome, test.relative)
			for _, name := range []string{"FUTURE_SETTING", "LD_PRELOAD", "FUTURE_EMPTY"} {
				if _, present := loaded.Environment[name]; present {
					t.Fatalf("%s reached the evaluated environment", name)
				}
				if _, present := loaded.Settings[name]; present {
					t.Fatalf("%s became a setting trace", name)
				}
			}
			if loaded.Environment["GIT_USER_NAME"] != "Example" {
				t.Fatalf("known setting beside unknown ones = %q", loaded.Environment["GIT_USER_NAME"])
			}
			var lines []int
			for _, assignment := range loaded.IgnoredAssignments() {
				if assignment.Unknown != UnknownSettingInput {
					continue
				}
				lines = append(lines, assignment.Line)
				text := assignment.String()
				if assignment.Path != path || assignment.Scope != test.scope || assignment.Repair() != "" ||
					!strings.Contains(text, assignment.Name) || !strings.Contains(text, path) {
					t.Fatalf("ignored assignment = %#v %q", assignment, text)
				}
				state := assignment.ConfigurationState()
				if state.Reason != string(UnknownSettingInput) || state.Message != text || state.Repair != "" {
					t.Fatalf("configuration state = %#v", state)
				}
				if strings.Contains(text, "future value") || strings.Contains(text, "/tmp/inject.so") {
					t.Fatalf("ignored assignment rendered its value: %q", text)
				}
			}
			slices.Sort(lines)
			if !slices.Equal(lines, []int{2, 3, 4}) {
				t.Fatalf("ignored unknown lines = %v", lines)
			}
		})
	}
}

func TestUnknownSettingsDoNotMaskIndependentRefusals(t *testing.T) {
	_, _, err := unknownSettingsFixture(t, map[string]string{
		"config.env": "FUTURE_SETTING=1\nSSH_PORT=70000\nFUTURE_TOKEN=password=hidden\n",
	})
	if err == nil || !strings.Contains(err.Error(), "must be in range") || !strings.Contains(err.Error(), "secret material") {
		t.Fatalf("refusals = %v", err)
	}
	if strings.Contains(err.Error(), "hidden") {
		t.Fatalf("refusal rendered a value: %v", err)
	}
}

func TestShippedDefaultsStillRefuseUnknownSettings(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "subyard.env")
	writeFixture(t, path, "FUTURE_SETTING=1\n")
	err := applyEnvFileValidated(path, environment{}, ScopeShipped, false, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("shipped unknown setting = %v", err)
	}
}

func TestUnknownSettingBecomesEffectiveWhenTheCatalogDefinesIt(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "config.env")
	writeFixture(t, path, "FUTURE_SETTING=enabled\n")
	var ignored []string
	values := environment{}
	if err := (Catalog{}).applyEnvFileValidatedFrom(path, path, values, ScopeHost, false, nil, func(name, _ string, _ int) { ignored = append(ignored, name) }); err != nil || values["FUTURE_SETTING"] != "" || !slices.Equal(ignored, []string{"FUTURE_SETTING"}) {
		t.Fatalf("older catalog: values=%v ignored=%v err=%v", values, ignored, err)
	}
	newer := Catalog{profiles: map[string]SettingDefinition{"FUTURE_SETTING": {
		Name: "FUTURE_SETTING", Kind: SettingScalar, Type: SettingString, Scopes: []SettingScope{ScopeHost}, Application: SettingNextCommand,
	}}}
	values, ignored = environment{}, nil
	if err := newer.applyEnvFileValidatedFrom(path, path, values, ScopeHost, false, nil, func(name, _ string, _ int) { ignored = append(ignored, name) }); err != nil || values["FUTURE_SETTING"] != "enabled" || len(ignored) != 0 {
		t.Fatalf("newer catalog: values=%v ignored=%v err=%v", values, ignored, err)
	}
}

func TestUnknownIntegrationsAreDroppedFromOperatorSelections(t *testing.T) {
	for _, test := range []struct{ relative, content string }{
		{"yards/default/config.env", "CODING_TOOL_INTEGRATIONS='futuretool'\n"},
		{".sync/settings/config.env", "CODING_TOOL_INTEGRATIONS='futuretool'\n"},
		{"config.env", "AGENTS=futuretool\n"},
	} {
		t.Run(test.relative, func(t *testing.T) {
			loaded, configHome, err := unknownSettingsFixture(t, map[string]string{test.relative: test.content})
			if err != nil {
				t.Fatal(err)
			}
			selection := loaded.Integrations
			if !selection.Present || len(selection.Requested) != 0 || len(selection.Effective) != 0 ||
				!slices.Equal(selection.Unknown, []string{"futuretool"}) || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "" {
				t.Fatalf("selection = %#v env=%q", selection, loaded.Environment["CODING_TOOL_INTEGRATIONS"])
			}
			var found bool
			for _, assignment := range loaded.IgnoredAssignments() {
				if assignment.Unknown == UnknownIntegrationInput {
					found = assignment.Item == "futuretool" && assignment.Path == filepath.Join(configHome, test.relative) &&
						assignment.Line == 1 && !strings.Contains(assignment.String(), "futuretool") && assignment.Repair() == ""
				}
			}
			if !found {
				t.Fatalf("ignored = %#v", loaded.IgnoredAssignments())
			}
		})
	}
}

func TestUnknownIntegrationKeepsKnownAndRefusesMalformedNames(t *testing.T) {
	loaded, _, err := unknownSettingsFixture(t, map[string]string{
		"config.env": "AGENT_custom_COMMAND=/usr/bin/true\nCODING_TOOL_INTEGRATIONS='futuretool custom'\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(loaded.Integrations.Requested, []string{"custom"}) || !slices.Equal(loaded.Integrations.Effective, []string{"custom"}) {
		t.Fatalf("selection = %#v", loaded.Integrations)
	}
	if _, _, err := unknownSettingsFixture(t, map[string]string{"config.env": "CODING_TOOL_INTEGRATIONS='bad/name'\n"}); err == nil || !strings.Contains(err.Error(), "invalid name value") {
		t.Fatalf("malformed integration name = %v", err)
	}
}

func TestUnknownProfilesAreNeverEffectiveAndSelectNoDefault(t *testing.T) {
	root := testkit.TempDir(t)
	for _, name := range []string{"declared", "legacy"} {
		writeFixture(t, filepath.Join(root, "config/profiles", name, "profile.conf"), "")
	}
	testkit.WriteFile(t, filepath.Join(root, "config/profiles/declared/profile.json"), []byte(`{"schema_version":1,"default_yards":["default"]}`), 0644)
	for _, test := range []struct{ content, effective string }{
		{"ENVIRONMENT_PROFILES=futureprofile\n", ""},
		{"ENVIRONMENT_PROFILES='legacy futureprofile'\n", "legacy"},
	} {
		home := filepath.Join(testkit.TempDir(t), "home")
		path := filepath.Join(home, "config/yards/default/config.env")
		writeFixture(t, path, test.content)
		loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "default", Environment: profileSelectionTestEnvironment(home)})
		if err != nil {
			t.Fatal(err)
		}
		selection := loaded.EnvironmentProfiles
		if !selection.Present || strings.Join(selection.Effective, " ") != test.effective || loaded.Environment["ENVIRONMENT_PROFILES"] != test.effective ||
			!slices.Contains(selection.Requested, "futureprofile") || !slices.Equal(selection.Unknown, []string{"futureprofile"}) {
			t.Fatalf("selection = %#v", selection)
		}
		ignored := loaded.IgnoredAssignments()
		if len(ignored) != 1 || ignored[0].Unknown != UnknownProfileInput || ignored[0].Item != "futureprofile" || ignored[0].Path != path || ignored[0].Line != 1 {
			t.Fatalf("ignored = %#v", ignored)
		}
		planned, err := WithEnvironmentProfiles(loaded, append(slices.Clone(selection.Requested), "legacy"))
		if err != nil || slices.Contains(planned.EnvironmentProfiles.Effective, "futureprofile") || slices.Contains(planned.EnvironmentProfiles.Effective, "declared") {
			t.Fatalf("planned selection = %#v %v", planned.EnvironmentProfiles, err)
		}
	}
}
