package profile

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestRevisionedSettingsPublicationMetadata(t *testing.T) {
	declaration := []byte(`{"schema_version":1,"instance_publication":{"schema_key":"user.subyard.synthetic_schema","schema_value":"1","revision_key":"user.subyard.synthetic_revision","disabled_setting":"SYN_ENABLED","legacy_settings_count":1,"settings":[{"name":"SYN_IMAGE"},{"name":"SYN_LIMIT","legacy_default":"one"}]}}`)
	metadata, err := Decode("synthetic", declaration)
	if err != nil {
		t.Fatal(err)
	}
	first, err := metadata.Data()
	if err != nil {
		t.Fatal(err)
	}
	first.InstancePublication.Settings[0].Name = "ALTERED"
	*first.InstancePublication.Settings[1].LegacyDefault = "changed"
	again, _ := metadata.Data()
	if again.InstancePublication.Settings[0].Name != "SYN_IMAGE" || *again.InstancePublication.Settings[1].LegacyDefault != "one" {
		t.Fatal("metadata projection aliases mutable declaration")
	}
	if err := ValidateMetadata([]Metadata{metadata, metadata}, nil); err == nil {
		t.Fatal("duplicate publication key ownership accepted")
	}
	integration, err := DecodeIntegration("synthetic", []byte(`{"schema_version":1,"provision":"missing.sh","lifecycle":{"handler":"missing.sh","instance_config_keys":["user.subyard.synthetic_schema"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateMetadata([]Metadata{metadata}, []IntegrationMetadata{integration}); err == nil {
		t.Fatal("integration publication key collision accepted")
	}
	for _, kind := range []string{"reserved", "foreign", "same-key", "bad-setting", "duplicate-setting", "missing-default", "legacy-count", "unsafe-default"} {
		t.Run(kind, func(t *testing.T) {
			data, _ := metadata.Data()
			publication := data.InstancePublication
			switch kind {
			case "reserved":
				publication.SchemaKey = "user.subyard.preview_sha256"
			case "foreign":
				publication.SchemaKey = "user.subyard.foreign_schema"
			case "same-key":
				publication.RevisionKey = publication.SchemaKey
			case "bad-setting":
				publication.DisabledSetting = "unsafe;command"
			case "duplicate-setting":
				publication.Settings[1].Name = publication.Settings[0].Name
			case "missing-default":
				publication.Settings[1].LegacyDefault = nil
			case "legacy-count":
				publication.LegacySettingsCount = 99
			case "unsafe-default":
				value := "unsafe\nvalue"
				publication.Settings[1].LegacyDefault = &value
			}
			encoded, err := json.Marshal(Definition{SchemaVersion: 1, Name: "synthetic", InstancePublication: publication})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Decode("synthetic", encoded); err == nil {
				t.Fatal("unsafe publication declaration accepted", kind)
			}
		})
	}
	// Only declared keys confer authority, never a namespace prefix.
	definition := metadata.definition()
	marker := map[string]string{"user.subyard.synthetic_schema": "1", "user.subyard.synthetic_revision": "0:" + strings.Repeat("a", 64) + ":images:debian/cloud"}
	projection, err := definition.ProjectInstancePublication(marker, marker, map[string]string{"SYN_ENABLED": "0", "SYN_IMAGE": "images:debian/cloud", "SYN_LIMIT": "one"})
	if err != nil || len(projection) != 2 {
		t.Fatal("synthetic publication failed", err)
	}
	if _, claimed := projection["user.subyard.synthetic_foreign"]; claimed {
		t.Fatal("prefix gained authority")
	}
}

func TestObservedPublicationUsesTypedHistoricalRecipeAndProtectedIdentity(t *testing.T) {
	legacyCPU := "2"
	definition := Definition{Name: "synthetic", InstancePublication: &RevisionedSettingsPublication{
		SchemaKey: "user.subyard.synthetic_schema", SchemaValue: "1", RevisionKey: "user.subyard.synthetic_revision", DisabledSetting: "SYN_ENABLED", LegacySettingsCount: 2,
		Settings: []PublicationSetting{{Name: "SYN_IMAGE"}, {Name: "SYN_YARD", FallbackSetting: "YARD_NAME"}, {Name: "SYN_CPU", LegacyDefault: &legacyCPU}},
	}}
	environment := map[string]string{"SYN_ENABLED": "0", "YARD_NAME": "fixture", "SYN_IMAGE": "images:debian/13", "SYN_CPU": "8"}
	for _, test := range []struct {
		recipe, cpu string
		valid       bool
	}{
		{"images:debian/13:fixture:4", "4", true}, {"images:debian/13:fixture", "2", true}, {"images:debian/13:foreign:4", "", false}, {"images:debian/13:fixture:bad", "", false},
	} {
		t.Run(test.recipe, func(t *testing.T) {
			marker := map[string]string{definition.InstancePublication.SchemaKey: "1", definition.InstancePublication.RevisionKey: "0:" + strings.Repeat("a", 64) + ":" + test.recipe}
			observed, err := definition.ObserveInstancePublication(marker, marker, environment, func(name, value string) error {
				if name == "SYN_IMAGE" && !strings.HasPrefix(value, "images:") {
					return errors.New("invalid image")
				}
				if name == "SYN_CPU" {
					number, err := strconv.Atoi(value)
					if err != nil || number < 1 || strconv.Itoa(number) != value {
						return errors.New("invalid CPU")
					}
				}
				return nil
			})
			if (err == nil) != test.valid {
				t.Fatalf("observed=%#v err=%v", observed, err)
			}
			if !test.valid {
				var verdict interface {
					MigrationRefusal() bool
					ActivationDiagnostic() (string, string)
				}
				if !errors.As(err, &verdict) || !verdict.MigrationRefusal() {
					t.Fatalf("publication verdict became dependency: %v", err)
				}
				message, _ := verdict.ActivationDiagnostic()
				if !strings.Contains(message, "yard fixture:") || !strings.Contains(message, "synthetic") || strings.Contains(message, test.recipe) {
					t.Fatalf("unsafe publication diagnostic: %q", message)
				}
			}
			if test.valid && !strings.Contains(observed[definition.InstancePublication.RevisionKey], `"SYN_CPU":"`+test.cpu+`"`) {
				t.Fatal("observation was replaced with current defaults")
			}
			if test.valid {
				if _, err := definition.ProjectInstancePublication(marker, marker, environment); err == nil {
					t.Fatal("strict continuation admitted desired recipe drift")
				}
			}
		})
	}
}

func TestAdoptionTakesHistoricalDisabledPublicationAsFound(t *testing.T) {
	legacy := "one"
	definition := Definition{Name: "synthetic", InstancePublication: &RevisionedSettingsPublication{
		SchemaKey: "user.subyard.synthetic_schema", SchemaValue: "1", RevisionKey: "user.subyard.synthetic_revision", DisabledSetting: "SYN_ENABLED", LegacySettingsCount: 1,
		Settings: []PublicationSetting{{Name: "SYN_IMAGE"}, {Name: "SYN_CPU", LegacyDefault: &legacy}},
	}}
	schema, revisionKey := definition.InstancePublication.SchemaKey, definition.InstancePublication.RevisionKey
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	historical := "0:" + first + ":" + second + ":4"
	accept := func(string, string) error { return nil }
	disabled := map[string]string{"SYN_ENABLED": "0", "SYN_IMAGE": "images:debian/13"}
	marker := map[string]string{revisionKey: historical}

	observed, err := definition.ObserveInstancePublication(marker, marker, disabled, accept)
	if err != nil || len(observed) != 1 || observed[revisionKey] != historical {
		t.Fatalf("historical disabled marker not taken as found: %#v %v", observed, err)
	}
	if _, err := definition.ProjectInstancePublication(marker, marker, disabled); err == nil {
		t.Fatal("strict continuation admitted the historical marker")
	}
	for name, test := range map[string]struct {
		local, effective, environment map[string]string
	}{
		"enabled mode":     {marker, marker, map[string]string{"SYN_ENABLED": "1"}},
		"unset mode":       {marker, marker, map[string]string{}},
		"enabled marker":   {map[string]string{revisionKey: "1:" + first + ":" + second + ":4"}, map[string]string{revisionKey: "1:" + first + ":" + second + ":4"}, map[string]string{"SYN_ENABLED": "1"}},
		"mode conflict":    {map[string]string{revisionKey: "1:" + first + ":" + second + ":4"}, map[string]string{revisionKey: "1:" + first + ":" + second + ":4"}, disabled},
		"schema present":   {map[string]string{revisionKey: historical, schema: "1"}, map[string]string{revisionKey: historical, schema: "1"}, disabled},
		"inherited schema": {marker, map[string]string{revisionKey: historical, schema: "1"}, disabled},
		"inherited marker": {map[string]string{}, marker, disabled},
		"expanded differs": {marker, map[string]string{revisionKey: "0:" + second + ":" + first + ":4"}, disabled},
		"short hash":       {map[string]string{revisionKey: "0:abc:" + second + ":4"}, map[string]string{revisionKey: "0:abc:" + second + ":4"}, disabled},
		"no cpu":           {map[string]string{revisionKey: "0:" + first + ":" + second}, map[string]string{revisionKey: "0:" + first + ":" + second}, disabled},
		"bad cpu":          {map[string]string{revisionKey: "0:" + first + ":" + second + ":0"}, map[string]string{revisionKey: "0:" + first + ":" + second + ":0"}, disabled},
		"extra field":      {map[string]string{revisionKey: historical + ":5"}, map[string]string{revisionKey: historical + ":5"}, disabled},
	} {
		t.Run(name, func(t *testing.T) {
			if observed, err := definition.ObserveInstancePublication(test.local, test.effective, test.environment, accept); err == nil {
				t.Fatalf("accepted: %#v", observed)
			}
		})
	}
}

func TestObservedPublicationRefusalBranches(t *testing.T) {
	legacy := "one"
	definition := Definition{Name: "synthetic", InstancePublication: &RevisionedSettingsPublication{SchemaKey: "user.subyard.synthetic_schema", SchemaValue: "1", RevisionKey: "user.subyard.synthetic_revision", DisabledSetting: "SYN_ENABLED", LegacySettingsCount: 1, Settings: []PublicationSetting{{Name: "SYN_IMAGE"}, {Name: "SYN_CPU", LegacyDefault: &legacy}}}}
	for _, branch := range []string{"revision malformed", "enabled"} {
		t.Run(branch, func(t *testing.T) {
			mode := "0"
			revision := "0:not-a-hash:images:debian/13:8"
			if branch == "enabled" {
				mode = "1"
				revision = "1:" + strings.Repeat("a", 64) + ":images:debian/13:8"
			}
			environment := map[string]string{"YARD_NAME": "fixture", "SYN_ENABLED": mode, "SYN_IMAGE": "images:debian/13", "SYN_CPU": "8"}
			marker := map[string]string{definition.InstancePublication.SchemaKey: "1", definition.InstancePublication.RevisionKey: revision}
			var err error
			if branch == "enabled" {
				_, err = definition.ProjectInstancePublication(marker, marker, environment)
			} else {
				_, err = definition.ObserveInstancePublication(marker, marker, environment, func(string, string) error { return nil })
			}
			expected := "native publication revision is malformed or differs from its protected mode"
			if branch == "enabled" {
				expected = "native publication is not supported while enabled"
			}
			var refusal interface {
				MigrationRefusal() bool
				ActivationDiagnostic() (string, string)
			}
			if err == nil || err.Error() != expected || !errors.As(err, &refusal) || !refusal.MigrationRefusal() {
				t.Fatalf("branch %s did not retain its verdict: %v", branch, err)
			}
			message, _ := refusal.ActivationDiagnostic()
			if !strings.Contains(message, expected) || !strings.Contains(message, "yard fixture") || strings.Contains(message, revision) {
				t.Fatalf("unsafe/missing publication reason: %q", message)
			}
		})
	}
}
