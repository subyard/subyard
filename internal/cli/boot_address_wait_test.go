package cli

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestRunBootPowerTreatsAddressWaitingAsTemporaryReadiness(t *testing.T) {
	reconciler := bootPowerFailureReconciler(errors.New("port collision"))
	fake := reconciler.Instances.(*testkit.Incus)
	instance := fake.Instances["p/yard"]
	instance.Devices = map[string]map[string]string{"synthetic": {
		"type": "proxy", "listen": "tcp:192.0.2.12:8080",
	}}
	fake.Instances["p/yard"] = instance
	var addresses []netip.Addr
	reconciler.LocalAddresses = func() ([]netip.Addr, error) { return addresses, nil }
	var stdout, stderr bytes.Buffer
	if code := RunBootPower(context.Background(), nil, &stdout, &stderr, reconciler); code != 75 ||
		stderr.Len() != 0 || !strings.Contains(stdout.String(), "waiting-for-address p/yard: 192.0.2.12") ||
		strings.Contains(stdout.String(), "no managed yards") {
		t.Fatalf("wait code/output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	addresses = []netip.Addr{netip.MustParseAddr("192.0.2.12")}
	stdout.Reset()
	if code := RunBootPower(context.Background(), nil, &stdout, &stderr, reconciler); code != 1 ||
		!strings.Contains(stderr.String(), "port collision") {
		t.Fatalf("permanent failure became retry: %q", stderr.String())
	}
}

func TestRunBootPowerWaitsWhenAddressDisappearsDuringStart(t *testing.T) {
	reconciler := bootPowerFailureReconciler(nil)
	fake := reconciler.Instances.(*testkit.Incus)
	instance := fake.Instances["p/yard"]
	instance.Devices = map[string]map[string]string{"synthetic": {
		"type": "proxy", "listen": "tcp:192.0.2.12:8080",
	}}
	fake.Instances["p/yard"] = instance
	addresses := []netip.Addr{netip.MustParseAddr("192.0.2.12")}
	reconciler.LocalAddresses = func() ([]netip.Addr, error) { return addresses, nil }
	starts := 0
	reconciler.Power = bootPowerManagerFunc(func(context.Context, string, string, string, bool) error {
		starts++
		addresses = nil
		return errors.New("bind: cannot assign requested address")
	})
	for range 2 {
		if code := RunBootPower(context.Background(), nil, &bytes.Buffer{}, &bytes.Buffer{}, reconciler); code != 75 {
			t.Fatalf("address loss returned %d", code)
		}
	}
	if starts != 1 {
		t.Fatalf("missing address caused repeated physical starts: %d", starts)
	}
	addresses = []netip.Addr{netip.MustParseAddr("192.0.2.12")}
	reconciler.Power = fake
	if code := RunBootPower(context.Background(), nil, &bytes.Buffer{}, &bytes.Buffer{}, reconciler); code != 0 ||
		len(fake.PowerUpdates) != 1 || !strings.EqualFold(fake.Instances["p/yard"].Status, "running") {
		t.Fatalf("address recovery failed: code=%d starts=%+v", code, fake.PowerUpdates)
	}
}

func TestRunBootPowerNetworkFailureTakesPrecedenceOverAddressWait(t *testing.T) {
	reconciler := bootPowerFailureReconciler(nil)
	fake := reconciler.Instances.(*testkit.Incus)
	instance := fake.Instances["p/yard"]
	instance.Devices = map[string]map[string]string{"synthetic": {
		"type": "proxy", "listen": "tcp:192.0.2.12:8080",
	}}
	fake.Instances["p/yard"] = instance
	reconciler.Network = bootAddressUnsafeNetwork{}
	reconciler.LocalAddresses = func() ([]netip.Addr, error) {
		t.Fatal("unsafe network proceeded to address observation")
		return nil, nil
	}
	if code := RunBootPower(context.Background(), nil, &bytes.Buffer{}, &bytes.Buffer{}, reconciler); code != 1 ||
		len(fake.PowerUpdates) != 0 {
		t.Fatalf("unsafe network became a wait: code=%d power=%+v", code, fake.PowerUpdates)
	}
}

type bootAddressUnsafeNetwork struct{}

func (bootAddressUnsafeNetwork) Check(context.Context, []string) error {
	return errors.New("unsafe host route")
}
