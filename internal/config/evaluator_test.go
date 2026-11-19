package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/profile"
)

func TestReaderLayerEvaluationPreservesLiteralProvenanceAndIsAtomic(t *testing.T) {
	base := map[string]string{"AGENT_synthetic_PERSIST": "base", "SSH_PORT": "2222"}
	result, err := (Catalog{}).EvaluateLayer(strings.NewReader("# comment\nAGENT_synthetic_PERSIST='first\nsecond'\nSSH_PORT=2223\nAGENT_synthetic_PERSIST='first second last'\n"), "logical.env", base, ScopeHost, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Values["AGENT_synthetic_PERSIST"] != "first second last" || result.Values["SSH_PORT"] != "2223" || base["AGENT_synthetic_PERSIST"] != "base" {
		t.Fatal("literal evaluation changed the base or lost the latest assignment")
	}
	if got := []int{result.Assignments[0].Line, result.Assignments[1].Line, result.Assignments[2].Line}; !reflect.DeepEqual(got, []int{2, 4, 5}) {
		t.Fatalf("provenance = %v", got)
	}
	for _, content := range []string{"AGENT_synthetic_PERSIST=changed\nSSH_PORT=0\n", "ACCESS_KIND=local\nYARD_TYPE=remote\n", "AGENT_synthetic_PERSIST=changed\nMISSING=${SSH_PORT:=2223}\n"} {
		_, err := (Catalog{}).EvaluateLayer(strings.NewReader(content), "logical.env", base, ScopeHost, false)
		var diagnostic *SourceError
		if err == nil || !errors.As(err, &diagnostic) || diagnostic.Path != "logical.env" || base["AGENT_synthetic_PERSIST"] != "base" {
			t.Fatalf("invalid layer escaped atomic validation: %v", err)
		}
	}
	values, err := EvaluateAssignments(strings.NewReader(": \"${FIRST:=one}\"\nSECOND=${FIRST}\nEMPTY=\n"), "logical.env", nil)
	if err != nil || values["FIRST"] != "one" || values["SECOND"] != "one" {
		t.Fatalf("default effects = %v, %v", values, err)
	}
	if value, exists := values["EMPTY"]; !exists || value != "" {
		t.Fatal("explicit empty disappeared")
	}
}

func TestBuildCatalogUsesRootFreeMetadata(t *testing.T) {
	metadata, err := profile.DecodeIntegration("synthetic", []byte(`{"schema_version":1,"inputs":[{"name":"SYNTHETIC_VERSION","type":"version","value":"1.2.3"}],"settings":[{"name":"SYNTHETIC_PORT","type":"port","scopes":["shipped","host"],"application":"yard-init","minimum":1,"maximum":65535,"default_from":{"setting":"SSH_PORT","offset":100}}],"provision":"missing.sh"}`))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := BuildCatalog(nil, []profile.IntegrationMetadata{metadata})
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := catalog.LookupSetting("SYNTHETIC_PORT")
	if !ok || definition.DefaultFrom == nil || definition.DefaultFrom.Setting != "SSH_PORT" {
		t.Fatal("derived metadata lost")
	}
	if err := catalog.ValidateSetting(ScopeHost, "SYNTHETIC_VERSION", "9.9.9", false); err == nil {
		t.Fatal("immutable input accepts host override")
	}
	if _, err := BuildCatalog([]profile.Metadata{{}}, nil); err == nil {
		t.Fatal("zero metadata accepted")
	}
}

func TestLocalBatchListenerChecksUseAvailableInputs(t *testing.T) {
	metadata, err := profile.Decode("fixture", []byte(`{
		"schema_version": 1, "settings": [
			{"name":"FIRST_PORT","type":"port","scopes":["host"],"application":"next-command","minimum":1024,"maximum":65535,"host_listener":true},
			{"name":"SECOND_PORT","type":"port","scopes":["host"],"application":"next-command","minimum":1024,"maximum":65535,"host_listener":true},
			{"name":"DERIVED_PORT","type":"port","scopes":["host"],"application":"next-command","minimum":1024,"maximum":65535,"host_listener":true,"default_from":{"setting":"SSH_PORT","offset":100}},
			{"name":"CHAIN_PORT","type":"port","scopes":["host"],"application":"next-command","minimum":1024,"maximum":65535,"host_listener":true,"default_from":{"setting":"DERIVED_PORT","offset":100}}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := BuildCatalog([]profile.Metadata{metadata}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		initial     environment
		unavailable []string
		want        string
	}{
		{"invalid SSH and transitive defaults", environment{
			"SSH_PORT": "2400", "ENVIRONMENT_PROFILES": "fixture",
			"FIRST_PORT": "2600", "SECOND_PORT": "2600", "WEB_PREVIEW_HOST_PORT": "2400",
		}, []string{"SSH_PORT"}, "SECOND_PORT collides with FIRST_PORT; choose another port"},
		{"invalid profile selection", environment{
			"SSH_PORT": "2400", "ENVIRONMENT_PROFILES": "fixture",
			"FIRST_PORT": "2400", "WEB_PREVIEW_HOST_PORT": "2400",
		}, []string{"ENVIRONMENT_PROFILES"}, "WEB_PREVIEW_HOST_PORT collides with SSH_PORT; choose another port"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := newSettingTracker()
			tracker.catalog = catalog
			tracker.batch = &localBatch{catalog: catalog, unavailable: map[string]bool{}}
			for _, name := range test.unavailable {
				tracker.batch.setAvailable(name, false)
			}
			err := tracker.batch.checkPorts("", LoadOptions{SyncSource: true}, domain.Context{SSHPort: 2400}, test.initial, tracker)
			if err == nil || err.Error() != test.want {
				t.Fatalf("port refusals = %v, want %q", err, test.want)
			}
		})
	}
}
