package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestEnvironmentProfileSelectionKeepsAuthoredIntentAndEffectiveSet(t *testing.T) {
	for _, tc := range []struct {
		name, yard, content, requested, effective string
		present                                   bool
	}{
		{name: "absent declared default", yard: "default", effective: "declared"},
		{name: "absent no defaults", yard: "named"},
		{name: "empty", yard: "default", content: "ENVIRONMENT_PROFILES=\n", present: true},
		{name: "exact descriptorless", yard: "default", content: "ENVIRONMENT_PROFILES=legacy\n", requested: "legacy", effective: "legacy", present: true},
		{name: "disabled", yard: "default", content: "ENVIRONMENT_PROFILES='declared legacy'\nNESTED_E2E_VMS=1\n", requested: "declared legacy", effective: "legacy", present: true},
		{name: "absent disabled", yard: "default", content: "NESTED_E2E_VMS=1\n"},
		{name: "lifecycle only", yard: "default", content: "ENVIRONMENT_PROFILES=cleanup\n", requested: "cleanup", present: true},
		{name: "legacy alias", yard: "default", content: "YARD_PROFILES=legacy\n", requested: "legacy", effective: "legacy", present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			if err := os.MkdirAll(filepath.Join(root, "config/profiles/declared"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "config/profiles/legacy"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "config/profiles/declared/profile.json"), []byte(`{"schema_version":1,"default_yards":["default"],"disabled_when":{"NESTED_E2E_VMS":"1"}}`), 0644)
			testkit.WriteFile(t, filepath.Join(root, "config/profiles/legacy/profile.conf"), nil, 0644)
			cleanup := filepath.Join(root, "config/profiles/cleanup")
			if err := os.MkdirAll(cleanup, 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(cleanup, "profile.json"), []byte(`{"schema_version":1,"lifecycle_only":true,"full_cleanup":"cleanup.sh"}`), 0644)
			testkit.WriteFile(t, filepath.Join(cleanup, "cleanup.sh"), []byte("#!/bin/sh\nexit 0\n"), 0755)
			home := filepath.Join(root, "home")
			path := filepath.Join(home, "config/yards", tc.yard, "config.env")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, []byte(tc.content), 0600)
			loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: tc.yard, DisablePrivate: true, Environment: profileSelectionTestEnvironment(home)})
			if err != nil {
				t.Fatal(err)
			}
			got := loaded.EnvironmentProfiles
			if got.Present != tc.present || strings.Join(got.Requested, " ") != tc.requested || strings.Join(got.Effective, " ") != tc.effective || loaded.Environment["ENVIRONMENT_PROFILES"] != tc.effective {
				t.Fatalf("selection=%#v effective env=%q", got, loaded.Environment["ENVIRONMENT_PROFILES"])
			}
			if tc.present && (got.Provenance.Path != path || got.Provenance.Scope != "yard") {
				t.Fatalf("provenance=%#v", got.Provenance)
			}
			if tc.requested == "cleanup" {
				if proposed, err := WithEnvironmentProfiles(loaded, []string{"cleanup"}); err != nil || len(proposed.EnvironmentProfiles.Effective) != 0 {
					t.Fatalf("lifecycle-only planned selection=%#v %v", proposed.EnvironmentProfiles, err)
				}
			}
			proposed, err := WithEnvironmentProfiles(loaded, []string{"declared", "legacy"})
			if err != nil {
				t.Fatal(err)
			}
			if !proposed.EnvironmentProfiles.Present || !slices.Equal(proposed.EnvironmentProfiles.Requested, []string{"declared", "legacy"}) || loaded.Environment["ENVIRONMENT_PROFILES"] != tc.effective {
				t.Fatalf("planned selection or original changed: %#v", proposed.EnvironmentProfiles)
			}
		})
	}
}

func TestEnvironmentProfileSelectionPreservesLayerPrecedence(t *testing.T) {
	root := testkit.TempDir(t)
	home := filepath.Join(root, "home")
	configHome := filepath.Join(home, "config")
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(content), 0600)
	}
	write(filepath.Join(root, "config/yards/profiles/preset.env"), "ENVIRONMENT_PROFILES=preset\n")
	write(filepath.Join(configHome, GitSettingsRelativePath, "yards/named/config.env"), "YARD_TEMPLATE=preset\nYARD_PROFILES=git\n")
	options := LoadOptions{RepositoryRoot: root, OperatorHome: home, YardName: "named", DisablePrivate: true, Environment: profileSelectionTestEnvironment(home)}
	check := func(want string) {
		t.Helper()
		loaded, err := Load(options)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Environment["ENVIRONMENT_PROFILES"] != want || strings.Join(loaded.EnvironmentProfiles.Requested, " ") != want {
			t.Fatalf("selection=%#v", loaded.EnvironmentProfiles)
		}
	}
	check("git")
	write(filepath.Join(configHome, "yards/named/config.env"), "YARD_PROFILES=local\n")
	check("local")
	options.Environment["ENVIRONMENT_PROFILES"] = "command"
	check("command")
	options.Environment["ENVIRONMENT_PROFILES"] = ""
	check("")
}

func profileSelectionTestEnvironment(home string) map[string]string {
	return map[string]string{"SUBYARD_CONFIG_HOME": filepath.Join(home, "config"), "DEV_UID": "1000", "SSH_PORT": "2222", "FORWARD_SSH_AGENT": "0", "DEV_SUDO": "0", "HOST_BASE": filepath.Join(home, "host"), "RESTRICTED_DISK_PATHS": filepath.Join(home, "host"), "STORAGE_PATH": filepath.Join(home, "storage"), "SHIFT_MODE": "shift"}
}

func TestEnvironmentProfileSelectionSuppressesIneligibleDefaultWithoutAcceptingExplicitRole(t *testing.T) {
	root := testkit.TempDir(t)
	directory := filepath.Join(root, "profiles/service")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(directory, "profile.conf"), []byte("PROVISION_SCOPE=dedicated\n"), 0644)
	catalog := Catalog{declarations: []profile.Definition{{Name: "service", DefaultYards: []string{"default"}}}}
	selection, err := resolveEnvironmentProfiles(root, "default", map[string]string{}, catalog)
	if err != nil || len(selection.Effective) != 0 || selection.Present {
		t.Fatalf("ineligible default=%#v %v", selection, err)
	}
	loaded := Loaded{Catalog: catalog, Context: domain.Context{YardName: "default", Paths: domain.RuntimePaths{ConfigDir: root}}, Environment: map[string]string{}}
	if _, err := WithEnvironmentProfiles(loaded, []string{"service"}); err == nil {
		t.Fatal("explicit dedicated request accepted on ordinary yard")
	}
}
