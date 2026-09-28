package config

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestDedicatedProfileConstraints(t *testing.T) {
	root := testkit.TempDir(t)
	profile := filepath.Join(root, "profiles", "service")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(profile, "profile.conf"), []byte("PROVISION_SCOPE=dedicated\n"), 0o644)
	base := environment{
		"YARD_KIND": "vm", "REQUIRED_YARD_KIND": "vm",
		"EXCLUSIVE_ENVIRONMENT_PROFILE": "service", "ENVIRONMENT_PROFILES": "service",
		"ALLOWS_HOST_ACCESS": "false", "SRV_VOLUME_TYPE": "block", "SRV_VOLUME_SIZE": "2GiB",
	}
	for _, test := range []struct {
		name, yard string
		values     environment
		wantError  bool
	}{
		{name: "dedicated VM", yard: "instance"},
		{name: "default yard", yard: "default", wantError: true},
		{name: "required kind", yard: "instance", values: environment{"YARD_KIND": "container"}, wantError: true},
		{name: "mixed profiles", yard: "instance", values: environment{"ENVIRONMENT_PROFILES": "service another"}, wantError: true},
		{name: "missing role", yard: "instance", values: environment{"EXCLUSIVE_ENVIRONMENT_PROFILE": ""}, wantError: true},
		{name: "host mount", yard: "instance", values: environment{"HOST_MOUNTS": "data:/mnt/data:ro:0700"}, wantError: true},
		{name: "agent forwarding", yard: "instance", values: environment{"FORWARD_SSH_AGENT": "1"}, wantError: true},
		{name: "unbounded state", yard: "instance", values: environment{"SRV_VOLUME_SIZE": ""}, wantError: true},
		{name: "deselected", yard: "instance", values: environment{"ENVIRONMENT_PROFILES": ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := maps.Clone(base)
			maps.Copy(values, test.values)
			if err := validateProfileConstraints(root, test.yard, values); (err != nil) != test.wantError {
				t.Fatalf("validateProfileConstraints() = %v, want error: %v", err, test.wantError)
			}
		})
	}
}

func TestResourceEndpointSettingsAreTypedWithoutAProfile(t *testing.T) {
	for _, test := range []struct {
		name, valid, invalid string
		typeOf               SettingValueType
	}{
		{"RESOURCE_EXAMPLE_IPV4", "192.0.2.1", "0.0.0.0", SettingIPv4},
		{"RESOURCE_EXAMPLE_INTERFACE", "eth0", "../eth0", SettingInterface},
		{"RESOURCE_EXAMPLE_PORT", "51820", "65536", SettingPort},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition, err := ValidateSettingName(ScopeCommand, test.name, false)
			if err != nil || definition.Type != test.typeOf {
				t.Fatalf("setting type = %v, %v; want %v", definition.Type, err, test.typeOf)
			}
			if err := ValidateSetting(ScopeCommand, test.name, test.valid, false); err != nil {
				t.Fatal(err)
			}
			if err := ValidateSetting(ScopeCommand, test.name, test.invalid, false); err == nil {
				t.Fatal("invalid endpoint setting accepted")
			}
		})
	}
}
