package testvmsruntime

import (
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
)

// The shipped declaration must admit the marker an old disabled backend left.
func TestDisabledBackendHistoricalMarkerEntersAdoptionAsFound(t *testing.T) {
	definitions, err := profile.Load("../../..")
	if err != nil {
		t.Fatal(err)
	}
	var owner *profile.Definition
	for index := range definitions {
		if definitions[index].Name == "test-vms" {
			owner = &definitions[index]
		}
	}
	if owner == nil || owner.InstancePublication == nil {
		t.Fatal("test-vms publication declaration is missing")
	}
	const key = "user.subyard.test_vms_revision"
	marker := map[string]string{key: "0:" + strings.Repeat("a", 64) + ":" + strings.Repeat("b", 64) + ":4"}
	accept := func(string, string) error { return nil }
	observed, err := owner.ObserveInstancePublication(marker, marker, map[string]string{"NESTED_E2E_VMS": "0"}, accept)
	if err != nil || observed[key] != marker[key] {
		t.Fatalf("disabled backend marker refused: %#v %v", observed, err)
	}
	if _, err := owner.ObserveInstancePublication(marker, marker, map[string]string{"NESTED_E2E_VMS": "1"}, accept); err == nil {
		t.Fatal("enabled backend admitted a historical disabled marker")
	}
	if _, err := owner.ProjectInstancePublication(marker, marker, map[string]string{"NESTED_E2E_VMS": "0"}); err == nil {
		t.Fatal("strict continuation admitted a historical disabled marker")
	}
}
