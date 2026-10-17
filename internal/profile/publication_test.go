package profile

import (
	"encoding/json"
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
