package releaseruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ownerservice"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRetainedReleasePreservesInfrastructureServiceContract(t *testing.T) {
	runtime, active, target, home, _ := retainedRuntimeFixture(t)
	defer runtime.Close()
	declaration := `{"schema_version":1,"runtime":{"activation_id":"sample-runtime","handler":"runtime.sh"},"infrastructure_service":{"id":"fixture-service","default_yard":"fixture-runtime","required_settings":{"YARD_KIND":"container"},"route_root":"fixture/routes"}}`
	for _, root := range []string{active, target} {
		testkit.WriteFile(t, filepath.Join(root, "config/profiles/sample/profile.json"), []byte(declaration), 0o600)
	}
	data := runtime.config.Inputs.environment(runtime.config.Context)["SUBYARD_HOME"]
	registry := ownerservice.Registry{Root: filepath.Join(data, "services")}
	record := ownerservice.Registration{Public: ownerservice.Public{SchemaVersion: 1, Owner: "fixture-owner", Service: "fixture-service", Pool: "main", Yard: "fixture-runtime", Project: "fixture-project", Instance: "fixture-instance", UUID: "fixture-uuid", Generation: strings.Repeat("a", 64)}, Ownership: strings.Repeat("b", 64)}
	if err := registry.Save(record); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil)
	if err != nil {
		t.Fatal(err)
	}
	record.Generation = strings.Repeat("c", 64)
	payload := []byte(strings.ReplaceAll(declaration, "fixture/routes", "another/routes"))
	testkit.WriteFile(t, filepath.Join(target, "config/profiles/sample/profile.json"), payload, 0o600)
	if _, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil); err == nil {
		t.Fatal("retained runtime lost installed service route contract")
	}
	testkit.WriteFile(t, filepath.Join(target, "config/profiles/sample/profile.json"), []byte(declaration), 0o600)
	path := filepath.Join(registry.Root, "fixture-service/main.json")
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte(strings.ReplaceAll(string(bytes), strings.Repeat("a", 64), strings.Repeat("c", 64))), 0o600)
	after, err := runtime.retainedRuntimeBinding(context.Background(), active, target, home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("service registration replacement did not invalidate retained admission")
	}
}
