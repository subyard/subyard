package testvms_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/profile"
)

func TestDisabledBackendPublication(t *testing.T) {
	payload, err := os.ReadFile("profile.json")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := profile.Decode("test-vms", payload)
	if err != nil {
		t.Fatal(err)
	}
	data, err := metadata.Data()
	if err != nil {
		t.Fatal(err)
	}
	owner := profile.Definition{Name: "test-vms", InstancePublication: data.InstancePublication}
	environment := map[string]string{"NESTED_E2E_VMS": "0", "E2E_VM_IMAGE": "images:debian/13/cloud", "E2E_VM_CPU": "2", "E2E_VM_MEMORY": "4GiB", "E2E_VM_DISK": "20GiB", "E2E_VM_SLOT_COUNT": "2", "E2E_VM_BOOT_TIMEOUT": "300", "YARD_NAME": "default", "E2E_DISK_BUDGET": "0GiB", "E2E_CACHE_BUDGET": "24GiB", "E2E_DISK_RESERVE": "5GiB", "E2E_MEMORY_RESERVE": "4GiB", "E2E_VM_OVERHEAD": "512MiB"}
	// Exact constructor envelope published in v0.8.0 backend.state. The image
	// colon is part of the setting, not a field boundary. Budget fields were
	// absent in that disabled backend, not historically enforced.
	old := "0:" + strings.Repeat("a", 64) + ":images:debian/13/cloud:2:4GiB:20GiB:2:300:default"
	current := "0:" + strings.Repeat("b", 64) + ":images:debian/13/cloud:2:4GiB:20GiB:2:300:default:0GiB:24GiB:5GiB:4GiB:512MiB"
	schemaKey, revisionKey := data.InstancePublication.SchemaKey, data.InstancePublication.RevisionKey
	project := func(marker string, env map[string]string) (map[string]string, error) {
		values := map[string]string{schemaKey: "1", revisionKey: marker}
		return owner.ProjectInstancePublication(values, values, env)
	}
	environment["E2E_BROKER_SOURCE"] = "default"
	if _, err := project(current, environment); err != nil {
		t.Fatal("explicit own source refused", err)
	}
	delete(environment, "E2E_BROKER_SOURCE")
	before, err := project(old, environment)
	if err != nil {
		t.Fatal("native old producer refused", err)
	}
	after, err := project(current, environment)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("current producer changed stable settings", err)
	}
	if before[schemaKey] != "1" || strings.Contains(before[revisionKey], strings.Repeat("a", 64)) {
		t.Fatal("schema lost or release hash retained")
	}
	for _, setting := range data.InstancePublication.Settings {
		if !strings.Contains(before[revisionKey], `"`+setting.Name+`"`) {
			t.Fatal("canonical setting omitted", setting.Name)
		}
	}
	for _, kind := range []string{"partial", "schema", "enabled", "uppercase", "trailing", "oversized", "bad-hash", "effective-only", "expanded-drift", "old-custom-budget", "foreign-source", "matching-foreign-source"} {
		t.Run(kind, func(t *testing.T) {
			local := map[string]string{schemaKey: "1", revisionKey: current}
			effective := map[string]string{schemaKey: "1", revisionKey: current}
			env := map[string]string{}
			for key, value := range environment {
				env[key] = value
			}
			switch kind {
			case "partial":
				delete(local, revisionKey)
				delete(effective, revisionKey)
			case "schema":
				local[schemaKey] = "2"
				effective[schemaKey] = "2"
			case "enabled":
				local[revisionKey] = "1" + current[1:]
				effective[revisionKey] = local[revisionKey]
			case "uppercase":
				local[revisionKey] = strings.ToUpper(current)
				effective[revisionKey] = local[revisionKey]
			case "trailing":
				local[revisionKey] += ":unknown"
				effective[revisionKey] = local[revisionKey]
			case "oversized":
				local[revisionKey] = strings.Repeat("x", 8193)
				effective[revisionKey] = local[revisionKey]
			case "bad-hash":
				local[revisionKey] = "0:invalid:" + strings.SplitN(current, ":", 3)[2]
				effective[revisionKey] = local[revisionKey]
			case "effective-only":
				delete(local, schemaKey)
				delete(local, revisionKey)
			case "expanded-drift":
				effective[revisionKey] = old
			case "old-custom-budget":
				local[revisionKey] = old
				effective[revisionKey] = old
				env["E2E_CACHE_BUDGET"] = "48GiB"
			case "foreign-source", "matching-foreign-source":
				env["E2E_BROKER_SOURCE"] = "foreign"
				if kind == "matching-foreign-source" {
					local[revisionKey] = strings.Replace(current, ":default:", ":foreign:", 1)
					effective[revisionKey] = local[revisionKey]
				}
			}
			if _, err := owner.ProjectInstancePublication(local, effective, env); err == nil {
				t.Fatal("unsafe publication accepted", kind)
			}
		})
	}
	for _, setting := range data.InstancePublication.Settings {
		t.Run(setting.Name, func(t *testing.T) {
			env := map[string]string{}
			for key, value := range environment {
				env[key] = value
			}
			env[setting.Name] = "changed"
			if _, err := project(current, env); err == nil {
				t.Fatal("changed setting accepted", setting.Name)
			}
		})
	}
	none, err := owner.ProjectInstancePublication(nil, nil, environment)
	if err != nil || len(none) != 0 {
		t.Fatal("absence gained metadata authority", err)
	}
	values := map[string]string{schemaKey: "1", revisionKey: current, "user.subyard.host_memory": "v1"}
	projected, err := owner.ProjectInstancePublication(values, values, environment)
	if err != nil {
		t.Fatal(err)
	}
	if _, owned := projected["user.subyard.host_memory"]; owned || len(projected) != 2 {
		t.Fatal("publication declaration claimed host access")
	}
}
