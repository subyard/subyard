package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
)

func TestReaderLayerEvaluationIsSequentialAndAtomic(t *testing.T) {
	base := map[string]string{"AGENT_synthetic_PERSIST": "base", "SSH_PORT": "2222"}
	result, err := (Catalog{}).EvaluateLayer(strings.NewReader("# comment\nAGENT_synthetic_PERSIST='first\nsecond'\nSSH_PORT=${SSH_PORT:=2223}\nAGENT_synthetic_PERSIST=\"${AGENT_synthetic_PERSIST} last\"\n"), "logical.env", base, ScopeHost, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Values["AGENT_synthetic_PERSIST"] != "first\nsecond last" || result.Values["SSH_PORT"] != "2222" || base["AGENT_synthetic_PERSIST"] != "base" {
		t.Fatal("sequential evaluation changed base or expansion")
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
