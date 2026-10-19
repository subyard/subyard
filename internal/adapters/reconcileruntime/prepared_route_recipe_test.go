package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func routeOriginalFixture(runtime Runtime) *testyardmigration.PreparedRouteDevices {
	source := filepath.Join(runtime.Yard.Paths.DataHome, "e2e/routes")
	return &testyardmigration.PreparedRouteDevices{Before: `{"schemaVersion":1,"active":true,"consumers":[{"project":"subyard","instance":"yard","yard":"default","mounted":false}]}`, Devices: []ports.PreparedReadOnlyDisk{{Project: "subyard", Instance: "yard", UUID: "original-uuid", Device: "subyard-e2e-routes", Source: source, Destination: "/var/lib/subyard/e2e-routes", Before: map[string]string{"type": "disk", "source": source, "path": "/var/lib/subyard/e2e-routes", "readonly": "true"}}}}
}

func TestRouteRecipeOriginalBytesFreshProcess(t *testing.T) {
	root := os.Getenv("SUBYARD_TEST_ORIGINAL_ROUTE_ROOT")
	child := root != ""
	if !child {
		root = testkit.TempDir(t)
	}
	runtime, _ := materializedIntegrationFixture(t, root, !child)
	runtime.Yard.Paths.DataHome = filepath.Join(root, "data-\xff")
	routes := routeOriginalFixture(runtime)
	path := filepath.Join(root, "original-route.json")
	if child {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		p, err := runtime.BindInstalledRuntimeContext(context.Background(), raw)
		if err != nil {
			t.Fatal("fresh original route bind", err)
		}
		defer p.Close()
		if !bytes.Equal(raw, p.Recipe()) || !reflect.DeepEqual(p.PreparedRoutes(), routes) {
			t.Fatal("fresh route authority changed")
		}
		return
	}
	p, err := runtime.PrepareRouteConsumerRecipe(context.Background(), routes, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	raw := p.Recipe()
	routes.Devices[0].Source = "changed after capture"
	routes.Devices[0].Before["source"] = "changed after capture"
	if !reflect.DeepEqual(p.PreparedRoutes(), routeOriginalFixture(runtime)) {
		t.Fatal("prepared route retained caller mutation authority")
	}
	testkit.WriteFile(t, path, raw, 0600)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRouteRecipeOriginalBytesFreshProcess$")
	cmd.Env = append(os.Environ(), "SUBYARD_TEST_ORIGINAL_ROUTE_ROOT="+root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh route bind: %v\n%s", err, output)
	}
	other := runtime
	other.Yard.Paths.DataHome = filepath.Join(root, "data-\xfe")
	q, err := other.PrepareRouteConsumerRecipe(context.Background(), routeOriginalFixture(other), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if p.RouteSourceBinding() == q.RouteSourceBinding() {
		t.Fatal("byte-distinct route paths share semantic source")
	}
	if _, err := other.BindInstalledRuntimeContext(context.Background(), raw); err == nil {
		t.Fatal("foreign route root admitted")
	}
	for _, change := range []string{"scope", "uuid", "rows"} {
		var wire installedRecipe
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		var d routeRecipe
		if err := nativejson.DecodeLosslessNativeJSON(wire.Files[0].Content, &d, 32<<20); err != nil {
			t.Fatal(err)
		}
		switch change {
		case "scope":
			d.Routes.Devices.Values[0].Source += "-foreign"
		case "uuid":
			d.Routes.Devices.Values[0].UUID = ""
		case "rows":
			d.Routes.Devices.Values[0].Before.Entries = append(d.Routes.Devices.Values[0].Before.Entries, d.Routes.Devices.Values[0].Before.Entries[0])
		}
		wire.Files[0].Content, err = nativejson.EncodeLosslessNativeJSON(d, 32<<20)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.BindInstalledRuntimeContext(context.Background(), changed); err == nil {
			t.Fatalf("%s malformed route authority admitted", change)
		}
	}
}
