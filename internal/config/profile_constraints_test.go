package config

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestProfileProvisionScopeRejectsAppendedProfilesSymlink(t *testing.T) {
	root := testkit.TempDir(t)
	target := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(target, "sample"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(target, "sample/profile.conf"), []byte("PROVISION_SCOPE=dedicated\n"), 0o644)
	if err := os.Symlink(target, filepath.Join(root, "profiles")); err != nil {
		t.Fatal(err)
	}
	if _, err := ProfileProvisionScope(root, "sample"); err == nil {
		t.Fatal("scope accepted appended profiles symlink")
	}
	if err := os.Remove(filepath.Join(root, "profiles")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "profiles/sample"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "profiles/sample/profile.conf"), []byte("PROVISION_SCOPE=dedicated\n"), 0o644)
	alias := filepath.Join(testkit.TempDir(t), "config-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if scope, err := ProfileProvisionScope(alias, "sample"); err != nil || scope != "dedicated" {
		t.Fatalf("explicit config entrypoint alias: scope=%q err=%v", scope, err)
	}
}

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

func TestVMCPUWeightContract(t *testing.T) {
	for _, value := range []string{"", "1", "100", "1000", "10000"} {
		if err := ValidateSetting(ScopeYard, "VM_CPU_WEIGHT", value, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"0", "-5", "10001", "high", "01000", "+1000"} {
		if err := ValidateSetting(ScopeYard, "VM_CPU_WEIGHT", value, false); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if err := validateProfileConstraints(testkit.TempDir(t), "synthetic", environment{"YARD_KIND": "container", "VM_CPU_WEIGHT": "1000"}); err == nil {
		t.Fatal("container accepted VM-only scheduling")
	}
}

func TestHostAccessProfileConstraintReportsEveryRefusal(t *testing.T) {
	root := testkit.TempDir(t)
	for name, body := range map[string]string{
		"route-client": "REQUIRES_HOST_ACCESS=1\n",
		"other-client": "REQUIRES_HOST_ACCESS=1\n",
		"plain":        "REQUIRES_HOST_ACCESS=0\n",
		"broken":       "REQUIRES_HOST_ACCESS=yes\nPROVISION_SCOPE=unknown\n",
	} {
		if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, "profiles", name, "profile.conf"), []byte(body), 0o644)
	}
	if err := validateProfileConstraints(root, "named", environment{"ENVIRONMENT_PROFILES": "route-client plain"}); err != nil {
		t.Fatalf("host-access yard refused a route profile: %v", err)
	}
	if err := validateProfileConstraints(root, "named", environment{"ENVIRONMENT_PROFILES": "plain", "ALLOWS_HOST_ACCESS": "false"}); err != nil {
		t.Fatalf("restricted yard refused a profile without the requirement: %v", err)
	}
	err := validateProfileConstraints(root, "named", environment{
		"ENVIRONMENT_PROFILES": "route-client other-client", "ALLOWS_HOST_ACCESS": "false", "HOST_LINKS": "x",
	})
	if err == nil {
		t.Fatal("restricted yard accepted profiles that require host access")
	}
	for _, want := range []string{
		`profile "route-client" requires host access`,
		`profile "other-client" requires host access`,
		"selected yard role forbids non-empty HOST_LINKS",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	err = validateProfileConstraints(root, "named", environment{"ENVIRONMENT_PROFILES": "broken"})
	if err == nil || !strings.Contains(err.Error(), "unsupported PROVISION_SCOPE") ||
		!strings.Contains(err.Error(), "unsupported REQUIRES_HOST_ACCESS") {
		t.Fatalf("invalid declarations were not all reported: %v", err)
	}
}
