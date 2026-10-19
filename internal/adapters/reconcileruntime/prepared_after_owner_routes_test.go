package reconcileruntime

import (
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
)

func TestAfterOwnerRoutesRetainOriginalConsumerScope(t *testing.T) {
	runtime := brokerRecipeFixture(t)
	routes := routeOriginalFixture(runtime)
	canonical := ports.PreparedReadOnlyDisk{Project: "subyard-test-yard", Instance: "yard-test-yard", UUID: "<after-owner>", Device: "subyard-e2e-routes", Source: filepath.Join(runtime.Yard.Paths.DataHome, "e2e/routes"), Destination: "/var/lib/subyard/e2e-routes"}
	routes.Devices = append([]ports.PreparedReadOnlyDisk{canonical}, routes.Devices...)
	routes.Broker = true
	p, err := runtime.PrepareAfterOwnerRoutes(context.Background(), routes, []byte(`{"version":1,"phase":"template","metadataBytes":4096,"descriptorBytes":4096}`))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.RouteSourceBinding() == "" || len(p.PreparedRoutes().Devices) != 2 {
		t.Fatal("reserved header lost original route scope")
	}
	current := canonical
	current.UUID = "original-created-uuid"
	current.Before = map[string]string{"type": "disk", "source": current.Source, "path": current.Destination, "readonly": "true"}
	resolved, physical, err := runtime.ResolveAfterOwnerRoutes(context.Background(), p.Recipe(), current)
	if err != nil {
		t.Fatal(err)
	}
	defer resolved.Close()
	exportAfterOwnerAdapterPair(t, "routes", p, physical, current.UUID, routes.Before)
	if len(physical) > AfterOwnerRoutePhysicalBound() {
		t.Fatal("physical record exceeded original scope bound")
	}
	if !reflect.DeepEqual(resolved.PreparedRoutes().Devices[1], routes.Devices[1]) {
		t.Fatal("non-owner consumer was recaptured")
	}
	if resolved.PreparedRoutes().Devices[0].UUID != current.UUID || !maps.Equal(resolved.PreparedRoutes().Devices[0].Before, current.Before) {
		t.Fatal("canonical causal physical view missing")
	}
	for _, change := range []string{"source", "device", "uuid"} {
		wrong := current
		switch change {
		case "source":
			wrong.Source += "-foreign"
		case "device":
			wrong.Device = "foreign"
		case "uuid":
			wrong.UUID = ""
		}
		if q, _, err := runtime.ResolveAfterOwnerRoutes(context.Background(), p.Recipe(), wrong); err == nil {
			q.Close()
			t.Fatal("foreign canonical tuple admitted", change)
		}
	}
	var record afterOwnerRoutePhysical
	if json.Unmarshal(physical, &record) != nil {
		t.Fatal("physical decode")
	}
	record.Instance = []byte("absent")
	bad, _ := json.Marshal(record)
	if q, err := runtime.BindAfterOwnerRoutes(context.Background(), p.Recipe(), bad); err == nil {
		q.Close()
		t.Fatal("unresolved absence admitted")
	}
}
