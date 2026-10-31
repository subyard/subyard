package profile

import "testing"

func TestInfrastructureServiceMetadata(t *testing.T) {
	content := []byte(`{"schema_version":1,"infrastructure_service":{"id":"fixture-service","default_yard":"service-container","required_settings":{"FIXTURE_ENABLED":"1"},"route_root":"fixture/routes"}}`)
	metadata, err := Decode("fixture", content)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := metadata.Data()
	data.InfrastructureService.RequiredSettings["FIXTURE_ENABLED"] = "changed"
	definition := metadata.definition()
	if definition.InfrastructureService.RequiredSettings["FIXTURE_ENABLED"] != "1" || !definition.InfrastructureService.Matches(map[string]string{"FIXTURE_ENABLED": "1"}) {
		t.Fatal("service metadata aliased or lost")
	}
	if definition.InfrastructureService.Matches(nil) {
		t.Fatal("disabled runtime accepted")
	}
	other, err := Decode("another", content)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateMetadata([]Metadata{metadata, other}, nil); err == nil {
		t.Fatal("duplicate infrastructure service accepted")
	}
	for _, invalid := range []string{
		`{"schema_version":1,"infrastructure_service":{"id":"fixture","default_yard":"default","required_settings":{"ENABLE":"1"},"route_root":"routes"}}`,
		`{"schema_version":1,"infrastructure_service":{"id":"fixture","default_yard":"service","required_settings":{"ENABLE":"1"},"route_root":"../escape"}}`,
		`{"schema_version":1,"infrastructure_service":{"id":"fixture","default_yard":"service","required_settings":{},"route_root":"routes"}}`,
	} {
		if _, err := Decode("fixture", []byte(invalid)); err == nil {
			t.Fatalf("unsafe metadata accepted: %s", invalid)
		}
	}
}
