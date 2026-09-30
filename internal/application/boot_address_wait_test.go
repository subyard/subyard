package application

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestBootPowerWaitsWithoutStartingAndRestoresAfterAddressAppears(t *testing.T) {
	waiting := managedPowerInstance("p", "a-waiting", "Stopped", PowerRunning)
	waiting.Devices = map[string]map[string]string{"synthetic": {
		"type": "proxy", "listen": "tcp:192.0.2.12:8080", "bind": "host",
	}}
	fake := &testkit.Incus{Instances: map[string]ports.InstanceInfo{
		"p/a-waiting": waiting,
		"p/z-ready":   managedPowerInstance("p", "z-ready", "Stopped", PowerRunning),
	}}
	var addresses []netip.Addr
	observations := 0
	reconciler := BootPowerReconciler{
		Inventory: fake, Instances: fake, Power: fake,
		Network:       networkGuardFunc(func(context.Context, []string) error { return nil }),
		NetworkPolicy: bootNetworkPolicyFunc(allowBootNetworkPolicy), EnsureNetworkLock: ensureBootNetworkLock,
		LocalAddresses: func() ([]netip.Addr, error) { observations++; return addresses, nil },
	}
	for range 2 {
		result, err := reconciler.Run(context.Background())
		if !errors.Is(err, ports.ErrHostAddressUnavailable) || len(result.Waiting) != 1 ||
			result.Waiting[0].Instance != "p/a-waiting" ||
			!reflect.DeepEqual(result.Waiting[0].Addresses, []string{"192.0.2.12"}) ||
			fake.Instances["p/z-ready"].Status != "Running" || fake.Instances["p/a-waiting"].Status != "Stopped" {
			t.Fatalf("unexpected wait: result=%+v err=%v instances=%+v", result, err, fake.Instances)
		}
	}
	if observations != 2 || len(fake.PowerUpdates) != 1 {
		t.Fatalf("waiting repeatedly started or polled: observations=%d starts=%+v", observations, fake.PowerUpdates)
	}
	addresses = []netip.Addr{netip.MustParseAddr("192.0.2.12")}
	result, err := reconciler.Run(context.Background())
	if err != nil || len(result.Waiting) != 0 || !reflect.DeepEqual(result.Started, []string{"p/a-waiting"}) ||
		fake.Instances["p/a-waiting"].Config["user.subyard.desired_power"] != PowerRunning || len(fake.PowerUpdates) != 2 {
		t.Fatalf("address appearance did not restore desired power once: %+v %v %+v", result, err, fake.PowerUpdates)
	}
}
