package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/testyardmigration"
)

func brokerRecipeFixture(t *testing.T) Runtime {
	t.Helper()
	runtime := shippedPreparedSourcesFixture(t)
	engine := filepath.Join(runtime.RepositoryRoot, "bin/yard-engine")
	if err := os.MkdirAll(filepath.Dir(engine), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, engine, []byte("approved original broker engine"), 0700)
	bin := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(bin, "incus"), []byte("#!/bin/sh\ncase \"$1 $2 $4\" in\n'config get volatile.uuid') printf original-uuid;;\n'config get '*) printf old;;\n'list '*) printf RUNNING;;\n*) exit 91;;\nesac\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	runtime.Yard.YardName = "test-yard"
	runtime.Yard.IncusProject = "subyard-test-yard"
	runtime.Yard.YardInstanceName = "yard-test-yard"
	runtime.Yard.IncusBridge = "incusbr0"
	runtime.Environment = append(runtime.Environment, "SUBYARD_DISPATCHER_PATH="+engine, "NESTED_E2E_VMS=0")
	incus := runningIncus(0)
	runtime.Incus = incus
	runtime.ConfigWriter = incus
	incus.Reconcile.Instance.Config = map[string]string{"volatile.uuid": "original-uuid"}
	incus.Instances = map[string]ports.InstanceInfo{runtime.Yard.IncusProject + "/" + runtime.Yard.YardInstanceName: incus.Reconcile.Instance}
	if err := (application.PowerService{Instances: incus, Config: incus}).Set(context.Background(), runtime.Yard, domain.PowerRunning, true); err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestBrokerAndRouteOriginalRecipesRebindWithoutRecapture(t *testing.T) {
	for _, kind := range []string{"broker", "route"} {
		t.Run(kind, func(t *testing.T) {
			runtime := brokerRecipeFixture(t)
			broker, err := runtime.PrepareTestVMBrokerRuntime(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close()
			var original *PreparedInstalledRuntime
			if kind == "broker" {
				original, err = runtime.PrepareTestVMBrokerRecipe(context.Background(), broker)
			} else {
				original, err = runtime.PrepareRouteConsumerRecipe(context.Background(), &testyardmigration.PreparedRouteDevices{Before: `{"schemaVersion":1,"active":true}`, Broker: true}, broker)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer original.Close()
			raw := original.Recipe()
			restored, err := runtime.BindInstalledRuntimeContext(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if !bytes.Equal(raw, restored.Recipe()) || restored.PreparedBroker().Binding() != broker.Binding() {
				t.Fatal("original broker authority was replaced")
			}
			if kind == "route" && restored.RouteSourceBinding() != original.RouteSourceBinding() {
				t.Fatal("original route source changed")
			}
			if kind == "broker" && restored.BrokerSourceBinding() != original.BrokerSourceBinding() {
				t.Fatal("original broker source changed")
			}
			testkit.WriteFile(t, filepath.Join(runtime.RepositoryRoot, "scripts/e2e-lab/provision.sh"), []byte("foreign source"), 0700)
			if _, err := runtime.BindInstalledRuntimeContext(context.Background(), raw); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatal("changed original source admitted", err)
			}
		})
	}
}

func TestRouteSourceExcludesOwnMutableBrokerAndDeviceBefore(t *testing.T) {
	runtime := brokerRecipeFixture(t)
	broker, err := runtime.PrepareTestVMBrokerRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	p, err := runtime.PrepareRouteConsumerRecipe(context.Background(), &testyardmigration.PreparedRouteDevices{Before: `{"schemaVersion":1,"active":true}`, Broker: true}, broker)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	q, err := runtime.PrepareRouteConsumerRecipe(context.Background(), &testyardmigration.PreparedRouteDevices{Before: `{"schemaVersion":1,"active":true}`, Broker: false}, broker)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if p.RouteSourceBinding() != q.RouteSourceBinding() {
		t.Fatal("completed original broker work changed immutable route source")
	}
	if bytes.Equal(p.Recipe(), q.Recipe()) {
		t.Fatal("original mutable work recipe was not bound separately")
	}
	restored, err := runtime.BindInstalledRuntimeContext(context.Background(), q.Recipe())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !bytes.Equal(restored.Recipe(), q.Recipe()) {
		t.Fatal("idle broker source recipe changed on restoration")
	}
}
