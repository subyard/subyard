package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/resource"
)

func TestLoadDefaultYardSettingsOverrideHostWithoutAffectingNamedYards(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	operatorHome := t.TempDir()
	configHome := filepath.Join(operatorHome, ".config", "subyard")
	writeFixture(t, filepath.Join(configHome, "config.env"), "ENVIRONMENT_PROFILES=android\n")
	writeFixture(t, filepath.Join(configHome, "yards", "default", "config.env"), "ENVIRONMENT_PROFILES=orca\n")
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
	if got := load("default").Environment["ENVIRONMENT_PROFILES"]; got != "orca" {
		t.Fatalf("default yard profile = %q, want orca", got)
	}
	if got := load("demo").Environment["ENVIRONMENT_PROFILES"]; got != "android" {
		t.Fatalf("named yard inherited default-yard settings: %q", got)
	}
	if err := os.Remove(filepath.Join(configHome, "yards", "default", "config.env")); err != nil {
		t.Fatal(err)
	}
	if got := load("default").Environment["ENVIRONMENT_PROFILES"]; got != "android" {
		t.Fatalf("default yard stopped inheriting host settings without its own file: %q", got)
	}
}

func TestDefaultYardCandidateUsesOnlyExplicitCandidateLayer(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	home := t.TempDir()
	configHome := filepath.Join(home, ".config", "subyard")
	writeFixture(t, filepath.Join(configHome, "yards", "default", "config.env"), "ENVIRONMENT_PROFILES=android\n")
	candidate := filepath.Join(home, "candidate.env")
	writeFixture(t, candidate, "ENVIRONMENT_PROFILES=orca\n")
	for _, test := range []struct {
		paths map[string]string
		want  string
	}{
		{map[string]string{"default": candidate}, "orca"},
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
	root := filepath.Clean(filepath.Join("..", ".."))
	operatorHome := t.TempDir()
	dataHome := filepath.Join(operatorHome, ".subyard")
	writeSavedEndpointFixture(t, dataHome, `{"schema":1,"allocations":[{"yard":"default","resource":"orca.orca","host":"owner.example.ts.net","port":17678}]}`)

	loaded, err := Load(LoadOptions{
		RepositoryRoot: root, OperatorHome: operatorHome, DisablePrivate: true,
		Environment: map[string]string{
			"SUBYARD_OPERATOR_HOME": operatorHome,
			"SUBYARD_HOME":          dataHome,
			"ENVIRONMENT_PROFILES":  "orca",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["ORCA_ADVERTISE_HOST"] != "owner.example.ts.net" ||
		loaded.Environment["ORCA_HOST_PORT"] != "17678" {
		t.Fatalf("saved endpoint was not loaded: %#v", loaded.Environment)
	}
	definition := orcaResourceDefinition(t, root)
	host, port := ResourceEndpointOverrides(loaded, definition)
	if host != "" || port != "" {
		t.Fatalf("saved allocation reported as an explicit override: host=%q port=%q", host, port)
	}
	for _, name := range []string{"ORCA_ADVERTISE_HOST", "ORCA_HOST_PORT"} {
		trace := loaded.Settings[name]
		if trace.EffectiveValue == "" || !strings.Contains(effectiveSettingDetail(trace), "saved endpoint allocation for orca.orca") {
			t.Fatalf("%s trace omitted saved allocation provenance: %#v", name, trace)
		}
	}
}

func TestLoadKeepsExplicitEndpointOverrides(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	operatorHome := t.TempDir()
	dataHome := filepath.Join(operatorHome, ".subyard")
	writeSavedEndpointFixture(t, dataHome, `{"schema":1,"allocations":[{"yard":"default","resource":"orca.orca","host":"saved.example.ts.net","port":17678}]}`)

	loaded, err := Load(LoadOptions{
		RepositoryRoot: root, OperatorHome: operatorHome, DisablePrivate: true,
		Environment: map[string]string{
			"SUBYARD_OPERATOR_HOME": operatorHome,
			"SUBYARD_HOME":          dataHome,
			"ENVIRONMENT_PROFILES":  "orca",
			"ORCA_ADVERTISE_HOST":   "explicit.example.ts.net",
			"ORCA_HOST_PORT":        "27678",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := orcaResourceDefinition(t, root)
	host, port := ResourceEndpointOverrides(loaded, definition)
	if host != "explicit.example.ts.net" || port != "27678" {
		t.Fatalf("explicit overrides were not preserved: host=%q port=%q", host, port)
	}
	if loaded.Environment["ORCA_ADVERTISE_HOST"] != host || loaded.Environment["ORCA_HOST_PORT"] != port {
		t.Fatalf("saved endpoint replaced explicit settings: %#v", loaded.Environment)
	}
}

func TestLoadSavedEndpointIsScopedToSelectedLocalYard(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	operatorHome := t.TempDir()
	configHome := filepath.Join(operatorHome, ".config", "subyard")
	dataHome := filepath.Join(operatorHome, ".subyard")
	writeSavedEndpointFixture(t, dataHome, `{"schema":1,"allocations":[{"yard":"demo","resource":"orca.orca","host":"demo.example.ts.net","port":17679},{"yard":"remote","resource":"orca.orca","host":"wrong.example.ts.net","port":17680}]}`)
	writeFixture(t, filepath.Join(configHome, "yards", "demo", "config.env"), "SSH_PORT=2223\n")
	writeFixture(t, filepath.Join(configHome, "yards", "remote", "config.env"), "ACCESS_KIND=remote\nOWNER_ENDPOINT=owner.example\nOWNER_YARD_NAME=default\n")

	tests := []struct {
		name, yard, profiles, wantHost, wantPort string
		syncSource                               bool
		layerPaths                               *LayerPaths
	}{
		{name: "selected named yard", yard: "demo", profiles: "orca", wantHost: "demo.example.ts.net", wantPort: "17679"},
		{name: "unselected profile", yard: "demo"},
		{name: "remote route", yard: "remote", profiles: "orca"},
		{name: "sync source", yard: "demo", profiles: "orca", syncSource: true},
		{name: "candidate layer paths", yard: "demo", profiles: "orca", layerPaths: &LayerPaths{}},
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
			if values["ORCA_ADVERTISE_HOST"] != test.wantHost || values["ORCA_HOST_PORT"] != test.wantPort {
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
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func orcaResourceDefinition(t *testing.T, root string) resource.Definition {
	t.Helper()
	registry, err := resource.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := registry.Lookup("orca")
	if !ok {
		t.Fatal("Orca resource definition is unavailable")
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
