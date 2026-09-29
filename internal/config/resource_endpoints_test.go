package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestLoadDefaultYardSettingsOverrideHostWithoutAffectingNamedYards(t *testing.T) {
	root := syntheticResourceRoot(t)
	operatorHome := testkit.TempDir(t)
	configHome := filepath.Join(operatorHome, ".config", "subyard")
	writeFixture(t, filepath.Join(configHome, "config.env"), "ENVIRONMENT_PROFILES=baseline\n")
	writeFixture(t, filepath.Join(configHome, "yards", "default", "config.env"), "ENVIRONMENT_PROFILES=fixture\n")
	writeFixture(t, filepath.Join(configHome, "yards", "demo", "config.env"), "SSH_PORT=2223\n")

	load := func(yard string) Loaded {
		t.Helper()
		loaded, err := Load(LoadOptions{
			RepositoryRoot: root, OperatorHome: operatorHome, YardName: yard, DisablePrivate: true,
			Environment: map[string]string{"SUBYARD_OPERATOR_HOME": operatorHome},
		})
		if err != nil {
			t.Fatal(err)
		}
		return loaded
	}
	if got := load("default").Environment["ENVIRONMENT_PROFILES"]; got != "fixture" {
		t.Fatalf("default yard profile = %q, want fixture", got)
	}
	if got := load("demo").Environment["ENVIRONMENT_PROFILES"]; got != "baseline" {
		t.Fatalf("named yard inherited default-yard settings: %q", got)
	}
	if err := os.Remove(filepath.Join(configHome, "yards", "default", "config.env")); err != nil {
		t.Fatal(err)
	}
	if got := load("default").Environment["ENVIRONMENT_PROFILES"]; got != "baseline" {
		t.Fatalf("default yard stopped inheriting host settings without its own file: %q", got)
	}
}

func TestDefaultYardCandidateUsesOnlyExplicitCandidateLayer(t *testing.T) {
	root := syntheticResourceRoot(t)
	home := testkit.TempDir(t)
	configHome := filepath.Join(home, ".config", "subyard")
	writeFixture(t, filepath.Join(configHome, "yards", "default", "config.env"), "ENVIRONMENT_PROFILES=baseline\n")
	candidate := filepath.Join(home, "candidate.env")
	writeFixture(t, candidate, "ENVIRONMENT_PROFILES=fixture\n")
	for _, test := range []struct {
		paths map[string]string
		want  string
	}{
		{map[string]string{"default": candidate}, "fixture"},
		{map[string]string{}, ""},
	} {
		loaded, err := Load(LoadOptions{RepositoryRoot: root, OperatorHome: home, DisablePrivate: true,
			Environment: map[string]string{"SUBYARD_OPERATOR_HOME": home},
			LayerPaths:  &LayerPaths{YardSettings: test.paths}})
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Environment["ENVIRONMENT_PROFILES"] != test.want {
			t.Fatalf("candidate profile=%q, want %q", loaded.Environment["ENVIRONMENT_PROFILES"], test.want)
		}
	}
}

func TestLoadUsesSavedEndpointForSelectedLocalResource(t *testing.T) {
	root := syntheticResourceRoot(t)
	operatorHome := testkit.TempDir(t)
	dataHome := filepath.Join(operatorHome, ".subyard")
	writeSavedEndpointFixture(t, dataHome, `{"schema":1,"allocations":[{"yard":"default","resource":"fixture.fixture","host":"owner.example.ts.net","port":17678}]}`)

	loaded, err := Load(LoadOptions{
		RepositoryRoot: root, OperatorHome: operatorHome, DisablePrivate: true,
		Environment: map[string]string{
			"SUBYARD_OPERATOR_HOME": operatorHome,
			"SUBYARD_HOME":          dataHome,
			"ENVIRONMENT_PROFILES":  "fixture",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["FIXTURE_ADVERTISE_HOST"] != "owner.example.ts.net" ||
		loaded.Environment["FIXTURE_HOST_PORT"] != "17678" {
		t.Fatalf("saved endpoint was not loaded: %#v", loaded.Environment)
	}
	definition := fixtureResourceDefinition(t, root)
	host, port := ResourceEndpointOverrides(loaded, definition)
	if host != "" || port != "" {
		t.Fatalf("saved allocation reported as an explicit override: host=%q port=%q", host, port)
	}
	for _, name := range []string{"FIXTURE_ADVERTISE_HOST", "FIXTURE_HOST_PORT"} {
		trace := loaded.Settings[name]
		if trace.EffectiveValue == "" || !strings.Contains(effectiveSettingDetail(trace), "saved endpoint allocation for fixture.fixture") {
			t.Fatalf("%s trace omitted saved allocation provenance: %#v", name, trace)
		}
	}
}

func TestLoadKeepsExplicitEndpointOverrides(t *testing.T) {
	root := syntheticResourceRoot(t)
	operatorHome := testkit.TempDir(t)
	dataHome := filepath.Join(operatorHome, ".subyard")
	writeSavedEndpointFixture(t, dataHome, `{"schema":1,"allocations":[{"yard":"default","resource":"fixture.fixture","host":"saved.example.ts.net","port":17678}]}`)

	loaded, err := Load(LoadOptions{
		RepositoryRoot: root, OperatorHome: operatorHome, DisablePrivate: true,
		Environment: map[string]string{
			"SUBYARD_OPERATOR_HOME":  operatorHome,
			"SUBYARD_HOME":           dataHome,
			"ENVIRONMENT_PROFILES":   "fixture",
			"FIXTURE_ADVERTISE_HOST": "explicit.example.ts.net",
			"FIXTURE_HOST_PORT":      "27678",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := fixtureResourceDefinition(t, root)
	host, port := ResourceEndpointOverrides(loaded, definition)
	if host != "explicit.example.ts.net" || port != "27678" {
		t.Fatalf("explicit overrides were not preserved: host=%q port=%q", host, port)
	}
	if loaded.Environment["FIXTURE_ADVERTISE_HOST"] != host || loaded.Environment["FIXTURE_HOST_PORT"] != port {
		t.Fatalf("saved endpoint replaced explicit settings: %#v", loaded.Environment)
	}
}

func TestLoadSavedEndpointIsScopedToSelectedLocalYard(t *testing.T) {
	root := syntheticResourceRoot(t)
	operatorHome := testkit.TempDir(t)
	configHome := filepath.Join(operatorHome, ".config", "subyard")
	dataHome := filepath.Join(operatorHome, ".subyard")
	writeSavedEndpointFixture(t, dataHome, `{"schema":1,"allocations":[{"yard":"demo","resource":"fixture.fixture","host":"demo.example.ts.net","port":17679},{"yard":"remote","resource":"fixture.fixture","host":"wrong.example.ts.net","port":17680}]}`)
	writeFixture(t, filepath.Join(configHome, "yards", "demo", "config.env"), "SSH_PORT=2223\n")
	writeFixture(t, filepath.Join(configHome, "yards", "remote", "config.env"), "ACCESS_KIND=remote\nOWNER_ENDPOINT=owner.example\nOWNER_YARD_NAME=default\n")

	tests := []struct {
		name, yard, profiles, wantHost, wantPort string
		syncSource                               bool
		layerPaths                               *LayerPaths
	}{
		{name: "selected named yard", yard: "demo", profiles: "fixture", wantHost: "demo.example.ts.net", wantPort: "17679"},
		{name: "unselected profile", yard: "demo"},
		{name: "remote route", yard: "remote", profiles: "fixture"},
		{name: "sync source", yard: "demo", profiles: "fixture", syncSource: true},
		{name: "candidate layer paths", yard: "demo", profiles: "fixture", layerPaths: &LayerPaths{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := environment{"ENVIRONMENT_PROFILES": test.profiles}
			ctx := resourceEndpointTestContext(root, operatorHome, configHome, dataHome, test.yard, test.yard != "remote")
			tracker := newSettingTracker()
			if err := applySavedResourceEndpoints(root, LoadOptions{
				SyncSource: test.syncSource, LayerPaths: test.layerPaths,
			}, ctx, values, tracker); err != nil {
				t.Fatal(err)
			}
			if values["FIXTURE_ADVERTISE_HOST"] != test.wantHost || values["FIXTURE_HOST_PORT"] != test.wantPort {
				t.Fatalf("endpoint scope mismatch: %#v", values)
			}
		})
	}
}

func writeSavedEndpointFixture(t *testing.T, dataHome, content string) {
	t.Helper()
	path := filepath.Join(dataHome, "resource-endpoints", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(content+"\n"), 0o600)
}

func syntheticResourceRoot(t *testing.T) string {
	t.Helper()
	root := testkit.TempDir(t)
	profile := filepath.Join(root, "config", "profiles", "fixture")
	if err := os.MkdirAll(filepath.Join(profile, "resources", "fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(profile, "profile.json"), []byte(`{"schema_version":1,"settings":[{"name":"FIXTURE_ADVERTISE_HOST","type":"string","scopes":["shipped","host","yard","command"],"application":"next-command","syncable":true,"optional":true},{"name":"FIXTURE_HOST_PORT","type":"port","scopes":["shipped","host","yard","command"],"application":"next-command","syncable":true,"optional":true,"minimum":1,"maximum":65535,"host_listener":true}]}`), 0o600)
	testkit.WriteFile(t, filepath.Join(profile, "resources", "fixture.res"), []byte("COMMAND=fixture\nTITLE=Fixture\nHANDLER=resources/fixture/handler.sh\nPROXY=\"fixture FIXTURE_ADVERTISE_HOST FIXTURE_HOST_PORT tcp:127.0.0.1:6768 loopback-or-tailscale\"\nENDPOINT_DEFAULTS=\"tailscale-self 6768\"\nBOOTSTRAP=profile\nACTION=\"up up bootstrap-change recreatable\"\nACTION=\"down down host-change reversible\"\nBRINGUP=up\nSHUTDOWN=down\n"), 0o600)
	testkit.WriteFile(t, filepath.Join(profile, "resources", "fixture", "handler.sh"), []byte("#!/bin/sh\n"), 0o700)
	testkit.WriteFile(t, filepath.Join(root, "config", "subyard.env"), []byte("SHIFT_MODE=shift\nDEV_UID=1000\nFORWARD_SSH_AGENT=0\nDEV_SUDO=0\nNESTED_E2E_VMS=0\nSTORAGE_PATH=$SUBYARD_HOME/storage\nHOST_BASE=$SUBYARD_HOME/host\nRESTRICTED_DISK_PATHS=$SUBYARD_HOME/host\nSSH_PORT=2222\n"), 0o600)
	return root
}

func fixtureResourceDefinition(t *testing.T, root string) resource.Definition {
	t.Helper()
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := registry.Lookup("fixture")
	if !ok {
		t.Fatal("fixture resource definition is unavailable")
	}
	return definition
}

func resourceEndpointTestContext(root, operatorHome, configHome, dataHome, yard string, local bool) domain.Context {
	access := domain.AccessRemote
	if local {
		access = domain.AccessLocal
	}
	return domain.Context{
		YardName: yard, AccessKind: access,
		Paths: domain.RuntimePaths{
			RepositoryRoot: root, OperatorHome: operatorHome,
			ConfigHome: configHome, DataHome: dataHome,
		},
	}
}
