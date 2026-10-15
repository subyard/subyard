package profile

import (
	"reflect"
	"testing"
)

func TestMetadataOwnsNestedSemanticValues(t *testing.T) {
	declaration := Definition{SchemaVersion: 1, Name: "synthetic", DisabledWhen: map[string]string{"OFF": "1"}, CredentialImportExclusions: [][]string{{"/keys/"}}, Settings: []Setting{{Name: "SYNTHETIC", Type: "string", Scopes: []string{"shipped"}, Application: "yard-init", Default: stringPointer("original")}}, Runtime: &RuntimeHook{ActivationID: "synthetic", Handler: "runtime.sh"}}
	metadata, err := declaration.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	declaration.DisabledWhen["OFF"] = "2"
	declaration.CredentialImportExclusions[0][0] = "/changed/"
	declaration.Settings[0].Scopes[0] = "yard"
	*declaration.Settings[0].Default = "changed"
	declaration.Runtime.Handler = "../escape"
	data, err := metadata.Data()
	if err != nil {
		t.Fatal(err)
	}
	if data.DisabledWhen["OFF"] != "1" || data.CredentialImportExclusions[0][0] != "/keys/" || *data.Settings[0].Default != "original" || data.Runtime.Handler != "runtime.sh" {
		t.Fatal("metadata aliases package data")
	}
	data.DisabledWhen["OFF"] = "3"
	*data.Settings[0].Default = "consumer changed"
	again, err := metadata.Data()
	if err != nil || again.DisabledWhen["OFF"] != "1" || *again.Settings[0].Default != "original" {
		t.Fatal("metadata aliases consumer snapshot")
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(MetadataData{}), reflect.TypeOf(IntegrationMetadataData{})} {
		if _, exists := typ.FieldByName("Root"); exists {
			t.Fatal("semantic metadata exposes a native root")
		}
	}
}

func TestFullCleanupMetadataRoundTripOwnsEnvironment(t *testing.T) {
	metadata, err := Decode("synthetic", []byte(`{"schema_version":1,"lifecycle_only":true,"full_cleanup":"missing-inspect.sh","full_cleanup_environment":["FIXTURE_ROOT"],"managed_paths":[{"root":"data","path":"routes/{yard}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := metadata.Data()
	if err != nil || !data.LifecycleOnly || data.FullCleanup != "missing-inspect.sh" {
		t.Fatalf("cleanup metadata lost: %#v, %v", data, err)
	}
	data.FullCleanupEnvironment[0] = "CHANGED"
	data.ManagedPaths[0].Path = "changed"
	definition := metadata.definition()
	if !definition.LifecycleOnly || definition.FullCleanupEnvironment[0] != "FIXTURE_ROOT" || definition.ManagedPaths[0].Path != "routes/{yard}" || definition.Selected("default", map[string]string{"ENVIRONMENT_PROFILES": "synthetic"}) {
		t.Fatal("cleanup ownership was lost or aliased")
	}
	definition.FullCleanupEnvironment[0] = "MUTATED"
	definition.ManagedPaths[0].Path = "mutated"
	again, _ := metadata.Data()
	if again.FullCleanupEnvironment[0] != "FIXTURE_ROOT" || again.ManagedPaths[0].Path != "routes/{yard}" {
		t.Fatal("native definition aliases semantic metadata")
	}
	for _, invalid := range []string{
		`{"schema_version":1,"lifecycle_only":true}`,
		`{"schema_version":1,"lifecycle_only":true,"full_cleanup":"inspect.sh","default_yards":["default"]}`,
		`{"schema_version":1,"lifecycle_only":true,"full_cleanup":"inspect.sh","setup":{}}`,
		`{"schema_version":1,"lifecycle_only":true,"full_cleanup":"inspect.sh","managed_paths":[{"root":"data","path":"../escape"}]}`,
		`{"schema_version":1,"lifecycle_only":true,"full_cleanup":"inspect.sh","managed_paths":[{"root":"foreign","path":"routes/{yard}"}]}`,
		`{"schema_version":1,"lifecycle_only":true,"full_cleanup":"inspect.sh","managed_paths":[{"root":"data","path":"routes/{unknown}"}]}`,
		`{"schema_version":1,"full_cleanup":"../escape"}`,
		`{"schema_version":1,"full_cleanup_environment":["FIXTURE_ROOT"]}`,
		`{"schema_version":1,"full_cleanup":"inspect.sh","full_cleanup_environment":["FIXTURE_ROOT","FIXTURE_ROOT"]}`,
		`{"schema_version":1,"full_cleanup":"inspect.sh","full_cleanup_environment":["unsafe-name"]}`,
	} {
		if _, err := Decode("synthetic", []byte(invalid)); err == nil {
			t.Fatalf("unsafe cleanup semantic declaration accepted: %s", invalid)
		}
	}
}

func TestIntegrationMetadataDoesNotRequireDeclaredFiles(t *testing.T) {
	metadata, err := DecodeIntegration("synthetic", []byte(`{"schema_version":1,"provision":"missing.sh","lifecycle":{"handler":"missing-handler.sh","source_files":["missing.data"],"instance_config_keys":["user.subyard.synthetic"],"instance_devices":["synthetic"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := metadata.Data()
	if err != nil {
		t.Fatal(err)
	}
	data.Lifecycle.SourceFiles[0] = "changed"
	data.Lifecycle.InstanceDevices[0] = "changed"
	again, err := metadata.Data()
	if err != nil || again.Lifecycle.SourceFiles[0] != "missing.data" || again.Lifecycle.InstanceDevices[0] != "synthetic" {
		t.Fatal("integration metadata aliases consumers")
	}
	for _, invalid := range []string{`{"schema_version":1} {}`, `{"schema_version":1,"lifecycle":{"handler":"handler.sh","source_files":["../escape"]}}`} {
		if _, err := DecodeIntegration("synthetic", []byte(invalid)); err == nil {
			t.Fatal("unsafe semantic input accepted")
		}
	}
	if err := ValidateMetadata(nil, []IntegrationMetadata{metadata, metadata}); err == nil {
		t.Fatal("native ownership collision accepted")
	}
}

func TestMetadataPreservesDashboardAndRetirementOwnership(t *testing.T) {
	metadata, err := Decode("synthetic", []byte(`{"schema_version":1,"owner_service":"missing-owner.sh","runtime":{"activation_id":"synthetic","handler":"missing-runtime.sh"},"settings":[{"name":"SAMPLE_PORT","type":"port","scopes":["yard"],"application":"yard-init","minimum":1024,"maximum":65535,"host_listener":true}],"dashboard":{"scheme":"http","path":"/","port_setting":"SAMPLE_PORT","guest_port":3000,"container_route":{"marker_key":"user.subyard.sample","device_name":"sample-dashboard"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := metadata.Data()
	data.Dashboard.ContainerRoute.DeviceName = "consumer-changed"
	if metadata.definition().Dashboard.ContainerRoute.DeviceName != "sample-dashboard" {
		t.Fatal("profile dashboard ownership was lost or aliased")
	}
	retired, err := DecodeIntegration("synthetic", []byte(`{"schema_version":1,"retired":true,"retired_settings":["OLD_PORT"],"retired_routes":[{"marker_key":"user.subyard.old","device_name":"old-dashboard","guest_port":3000}],"lifecycle":{"handler":"missing-retire.py","instance_config_keys":["user.subyard.old"],"instance_devices":["old-dashboard"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	old, _ := retired.Data()
	old.RetiredSettings[0] = "CHANGED"
	old.RetiredRoutes[0].DeviceName = "consumer-changed"
	definition := retired.definition()
	if !definition.Retired || definition.RetiredSettings[0] != "OLD_PORT" || definition.RetiredRoutes[0].DeviceName != "old-dashboard" {
		t.Fatal("retirement cleanup ownership was lost or aliased")
	}
	active, err := DecodeIntegration("synthetic", []byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateMetadata(nil, []IntegrationMetadata{retired, active}); err == nil {
		t.Fatal("active and retired metadata with the same name accepted")
	}
	for _, invalid := range []string{
		`{"schema_version":1,"retired":true,"provision":"active.sh","lifecycle":{"handler":"retire.py"}}`,
		`{"schema_version":1,"retired":true,"retired_routes":[{"marker_key":"user.subyard.foreign","device_name":"foreign","guest_port":3000}],"lifecycle":{"handler":"retire.py"}}`,
	} {
		if _, err := DecodeIntegration("synthetic", []byte(invalid)); err == nil {
			t.Fatal("retired metadata gained active or foreign authority")
		}
	}
}
