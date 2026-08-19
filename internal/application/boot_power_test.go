package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type networkGuardFunc func(context.Context, []string) error

type inventoryFunc func(context.Context) ([]ports.InstanceInfo, error)

func (function inventoryFunc) ListInstances(ctx context.Context) ([]ports.InstanceInfo, error) {
	return function(ctx)
}

type advancingClock struct{ now time.Time }

func (clock *advancingClock) Now() time.Time { return clock.now }

func (clock *advancingClock) After(delay time.Duration) <-chan time.Time {
	clock.now = clock.now.Add(delay)
	ready := make(chan time.Time, 1)
	ready <- clock.now
	return ready
}

func (function networkGuardFunc) Check(ctx context.Context, bridges []string) error {
	return function(ctx, bridges)
}

func managedPowerInstance(project, name, status, desired string) ports.InstanceInfo {
	return ports.InstanceInfo{
		Project: project, Name: name, Status: status,
		Config: map[string]string{
			"user.subyard.managed": "true", "user.subyard.initialized": "true",
			"user.subyard.desired_power": desired, "user.subyard.bridge": "incusbr0",
			"boot.autostart": "false",
		},
	}
}

func TestBootPowerMetadataPhaseMatrix(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		value     string
		delete    bool
		wantCount int
		wantErr   bool
	}{
		{name: "valid", wantCount: 1},
		{name: "managed absent", key: "user.subyard.managed", delete: true},
		{name: "managed empty", key: "user.subyard.managed"},
		{name: "managed false", key: "user.subyard.managed", value: "false"},
		{name: "managed malformed", key: "user.subyard.managed", value: "yes", wantErr: true},
		{name: "initialized absent", key: "user.subyard.initialized", delete: true, wantErr: true},
		{name: "initialized empty", key: "user.subyard.initialized", wantErr: true},
		{name: "initialized false", key: "user.subyard.initialized", value: "false", wantErr: true},
		{name: "desired absent", key: "user.subyard.desired_power", delete: true, wantErr: true},
		{name: "desired malformed", key: "user.subyard.desired_power", value: "paused", wantErr: true},
		{name: "name absent is ignored", key: "user.subyard.name", delete: true, wantCount: 1},
		{name: "bridge absent", key: "user.subyard.bridge", delete: true, wantErr: true},
		{name: "bridge empty", key: "user.subyard.bridge", wantErr: true},
		{name: "autostart absent", key: "boot.autostart", delete: true, wantErr: true},
		{name: "autostart true", key: "boot.autostart", value: "true", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := managedPowerInstance("p", "yard", "Stopped", PowerStopped)
			if test.key != "" {
				if test.delete {
					delete(instance.Config, test.key)
				} else {
					instance.Config[test.key] = test.value
				}
			}
			managed, err := filterManagedInstances([]ports.InstanceInfo{instance}, true)
			if len(managed) != test.wantCount || (err != nil) != test.wantErr {
				t.Fatalf("managed=%#v error=%v, want count=%d error=%v",
					managed, err, test.wantCount, test.wantErr)
			}
		})
	}
}

func TestBootPowerReconcilerRestoresDesiredState(t *testing.T) {
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"p/already": managedPowerInstance("p", "already", "Running", PowerRunning),
		"p/start":   managedPowerInstance("p", "start", "Stopped", PowerRunning),
		"p/stop":    managedPowerInstance("p", "stop", "Running", PowerStopped),
	}}
	var checks [][]string
	reconciler := BootPowerReconciler{
		Inventory: fake, Instances: fake, Power: fake,
		Network: networkGuardFunc(func(_ context.Context, bridges []string) error {
			checks = append(checks, append([]string(nil), bridges...))
			return nil
		}),
	}
	result, err := reconciler.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Started, []string{"p/start"}) ||
		!reflect.DeepEqual(result.Stopped, []string{"p/stop"}) ||
		!reflect.DeepEqual(result.AlreadyRunning, []string{"p/already"}) {
		t.Fatalf("unexpected result: %#v", result)
	}
	wantUpdates := []testkit.InstancePowerUpdate{
		{Project: "p", Name: "stop", Action: "stop"},
		{Project: "p", Name: "start", Action: "start"},
	}
	if !reflect.DeepEqual(fake.PowerUpdates, wantUpdates) {
		t.Fatalf("unexpected power updates: %#v", fake.PowerUpdates)
	}
	if len(checks) != 3 || !reflect.DeepEqual(checks[0], []string{"incusbr0"}) {
		t.Fatalf("unexpected network checks: %#v", checks)
	}
}

func TestBootPowerReconcilerRejectsInvalidMetadataBeforeMutation(t *testing.T) {
	invalid := managedPowerInstance("p", "yard", "Stopped", PowerRunning)
	invalid.Config["boot.autostart"] = "true"
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"p/yard": invalid}}
	reconciler := BootPowerReconciler{
		Inventory: fake, Instances: fake, Power: fake,
		Network: networkGuardFunc(func(context.Context, []string) error { return nil }),
	}
	_, err := reconciler.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boot.autostart=false") {
		t.Fatalf("expected metadata error, got %v", err)
	}
	if len(fake.PowerUpdates) != 0 {
		t.Fatalf("invalid metadata caused mutations: %#v", fake.PowerUpdates)
	}
}

func TestBootPowerReconcilerStopsRunningYardsAfterGuardFailure(t *testing.T) {
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"p/first":  managedPowerInstance("p", "first", "Running", PowerRunning),
		"p/second": managedPowerInstance("p", "second", "Stopped", PowerRunning),
	}}
	checks := 0
	reconciler := BootPowerReconciler{
		Inventory: fake, Instances: fake, Power: fake,
		Network: networkGuardFunc(func(context.Context, []string) error {
			checks++
			if checks == 3 {
				return errors.New("unsafe route")
			}
			return nil
		}),
	}
	_, err := reconciler.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "running yards stopped fail-closed") {
		t.Fatalf("expected fail-closed error, got %v", err)
	}
	want := []testkit.InstancePowerUpdate{
		{Project: "p", Name: "second", Action: "start"},
		{Project: "p", Name: "first", Action: "stop", Force: true},
		{Project: "p", Name: "second", Action: "stop", Force: true},
	}
	if !reflect.DeepEqual(fake.PowerUpdates, want) {
		t.Fatalf("unexpected fail-closed updates: %#v", fake.PowerUpdates)
	}
}

func TestBootPowerReconcilerReReadsIntentBeforeStart(t *testing.T) {
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"p/yard": managedPowerInstance("p", "yard", "Stopped", PowerRunning),
	}}
	reconciler := BootPowerReconciler{
		Inventory: fake, Instances: fake, Power: fake,
		Network: networkGuardFunc(func(context.Context, []string) error {
			instance := fake.Instances["p/yard"]
			instance.Config["user.subyard.desired_power"] = PowerStopped
			fake.Instances["p/yard"] = instance
			return nil
		}),
	}
	result, err := reconciler.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Started) != 0 || len(fake.PowerUpdates) != 0 {
		t.Fatalf("stale intent caused a start: %#v %#v", result, fake.PowerUpdates)
	}
}

func TestBootPowerReconcilerHasManaged(t *testing.T) {
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"p/unmanaged": {Project: "p", Name: "unmanaged"},
	}}
	reconciler := BootPowerReconciler{Inventory: fake}
	managed, err := reconciler.HasManaged(context.Background())
	if err != nil || managed {
		t.Fatalf("unexpected unmanaged result: %v, %v", managed, err)
	}
	fake.Instances["p/yard"] = managedPowerInstance("p", "yard", "Stopped", PowerStopped)
	managed, err = reconciler.HasManaged(context.Background())
	if err != nil || !managed {
		t.Fatalf("unexpected managed result: %v, %v", managed, err)
	}
}

func TestBootPowerReconcilerWaitsForIncusInventory(t *testing.T) {
	instance := managedPowerInstance("p", "yard", "Stopped", PowerRunning)
	attempts := 0
	clock := &advancingClock{now: time.Unix(1_000, 0)}
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"p/yard": instance}}
	reconciler := BootPowerReconciler{
		Inventory: inventoryFunc(func(context.Context) ([]ports.InstanceInfo, error) {
			attempts++
			if attempts < 3 {
				return nil, errors.New("Incus is starting")
			}
			return []ports.InstanceInfo{instance}, nil
		}),
		Instances: fake, Power: fake,
		Network: networkGuardFunc(func(context.Context, []string) error { return nil }),
		Clock:   clock, IncusWait: 5 * time.Second,
	}
	result, err := reconciler.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || !reflect.DeepEqual(result.Started, []string{"p/yard"}) {
		t.Fatalf("unexpected readiness result: attempts=%d result=%#v", attempts, result)
	}
}

func TestBootPowerReconcilerTimesOutWaitingForIncus(t *testing.T) {
	clock := &advancingClock{now: time.Unix(1_000, 0)}
	reconciler := BootPowerReconciler{
		Inventory: inventoryFunc(func(context.Context) ([]ports.InstanceInfo, error) {
			return nil, errors.New("offline")
		}),
		Instances: &testkit.Incus{}, Power: &testkit.Incus{},
		Network: networkGuardFunc(func(context.Context, []string) error { return nil }),
		Clock:   clock, IncusWait: 2 * time.Second,
	}
	_, err := reconciler.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "within 2s") {
		t.Fatalf("expected readiness timeout, got %v", err)
	}
}

func TestBootPowerReconcilerBoundsBlockedIncusInventory(t *testing.T) {
	reconciler := BootPowerReconciler{
		Inventory: inventoryFunc(func(ctx context.Context) ([]ports.InstanceInfo, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}),
		Instances: &testkit.Incus{}, Power: &testkit.Incus{},
		Network:   networkGuardFunc(func(context.Context, []string) error { return nil }),
		IncusWait: 20 * time.Millisecond,
	}
	result := make(chan error, 1)
	go func() {
		_, err := reconciler.Run(context.Background())
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, ports.ErrIncusUnavailable) ||
			!strings.Contains(err.Error(), "within 20ms") {
			t.Fatalf("blocked readiness error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Incus inventory outlived its readiness budget")
	}
}
