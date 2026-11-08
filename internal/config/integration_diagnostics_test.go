package config

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestIntegrationDiagnosticDigestsKeepValuesPrivateAndDetectChanges(t *testing.T) {
	catalog := Catalog{profiles: map[string]SettingDefinition{
		"SECRET": {Name: "SECRET", Kind: SettingScalar, Sensitive: true, Application: SettingYardInit, Owner: "integration:sample"},
		"FILE":   {Name: "FILE", Kind: SettingFile, Application: SettingYardInit, Owner: "integration:sample"},
	}}
	key := bytes.Repeat([]byte{1}, 32)
	values := map[string]string{"SECRET": "secret-one", "FILE": "/same"}
	content := []byte("first file")
	read := func(MaterializedAsset) ([]byte, error) { return content, nil }
	first := catalog.IntegrationDiagnosticDigests(values, key, read)
	if len(first) != 2 || !catalog.ValidIntegrationDiagnosticEvidence(strings.Repeat("01", 32), first) {
		t.Fatalf("invalid evidence: %#v", first)
	}
	values["SECRET"] = "secret-two"
	content = []byte("second file")
	second := catalog.IntegrationDiagnosticDigests(values, key, read)
	if first["SECRET"] == second["SECRET"] || first["FILE"] == second["FILE"] {
		t.Fatal("changed inputs had unchanged evidence")
	}
	for _, digest := range second {
		if strings.Contains(digest, "secret") || strings.Contains(digest, "file") {
			t.Fatal("evidence contains input")
		}
	}
	missing := catalog.IntegrationDiagnosticDigests(values, key, func(MaterializedAsset) ([]byte, error) { return nil, errors.New("unreadable") })
	if _, ok := missing["FILE"]; ok {
		t.Fatal("unreadable file acquired guessed evidence")
	}
}
