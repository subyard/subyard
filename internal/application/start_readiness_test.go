package application

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestStartAddressReadiness(t *testing.T) {
	for _, test := range []struct {
		name, listen, bind, nat, status, desired string
		local                                    []netip.Addr
		want                                     []string
		wantError                                bool
	}{
		{name: "missing IPv4", listen: "tcp:192.0.2.12:8080", want: []string{"192.0.2.12"}},
		{name: "appeared IPv4", listen: "tcp:192.0.2.12:8080", local: []netip.Addr{netip.MustParseAddr("192.0.2.12")}},
		{name: "IPv6 UDP", listen: "udp:[2001:db8::12]:8080", want: []string{"2001:db8::12"}},
		{name: "IPv6 appeared", listen: "tcp:[2001:db8::12]:8080", local: []netip.Addr{netip.MustParseAddr("2001:db8::12")}},
		{name: "scoped IPv6 appeared", listen: "tcp:[fe80::12%eth0]:8080", local: []netip.Addr{netip.MustParseAddr("fe80::12%eth0")}},
		{name: "scoped IPv6 on another interface", listen: "tcp:[fe80::12%eth0]:8080", local: []netip.Addr{netip.MustParseAddr("fe80::12%eth1")}, want: []string{"fe80::12%eth0"}},
		{name: "port range", listen: "tcp:192.0.2.12:8080-8082,8090", want: []string{"192.0.2.12"}},
		{name: "loopback", listen: "tcp:127.0.0.2:8080"},
		{name: "loopback IPv6", listen: "tcp:[::1]:8080"},
		{name: "wildcard", listen: "tcp:0.0.0.0:8080"},
		{name: "wildcard IPv6", listen: "tcp:[::]:8080"},
		{name: "unix", listen: "unix:/run/example.sock"},
		{name: "instance binding", listen: "tcp:192.0.2.12:8080", bind: "instance"},
		{name: "NAT has no bound socket", listen: "tcp:192.0.2.12:8080", nat: "true"},
		{name: "running", listen: "tcp:192.0.2.12:8080", status: "Running"},
		{name: "desired stopped", listen: "tcp:192.0.2.12:8080", desired: PowerStopped},
		{name: "malformed", listen: "tcp:192.0.2.12", wantError: true},
		{name: "non-literal", listen: "tcp:example.invalid:8080", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance := managedPowerInstance("p", "yard", "Stopped", PowerRunning)
			if test.status != "" {
				instance.Status = test.status
			}
			if test.desired != "" {
				instance.Config["user.subyard.desired_power"] = test.desired
			}
			// Effective devices include inherited proxies; no local-device lookup is needed.
			instance.Devices = map[string]map[string]string{"synthetic": {
				"type": "proxy", "listen": test.listen, "bind": test.bind, "nat": test.nat,
			}}
			calls := 0
			state, missing, err := StartAddressReadiness(instance, func() ([]netip.Addr, error) {
				calls++
				return test.local, nil
			})
			if (err != nil) != test.wantError || !reflect.DeepEqual(missing, test.want) {
				t.Fatalf("state=%s addresses=%v err=%v", state, missing, err)
			}
			if (state == domain.StartWaitingForAddress) != (len(test.want) != 0) || calls > 1 {
				t.Fatalf("unexpected state or polling: state=%s calls=%d", state, calls)
			}
		})
	}
}

func TestStartAddressReadinessDoesNotFabricateWaitingOnObservationFailure(t *testing.T) {
	instance := managedPowerInstance("p", "yard", "Stopped", PowerRunning)
	instance.Devices = map[string]map[string]string{
		"one":   {"type": "proxy", "listen": "tcp:192.0.2.12:8080"},
		"two":   {"type": "proxy", "listen": "udp:192.0.2.12:8090"},
		"three": {"type": "proxy", "listen": "tcp:192.0.2.11:8080"},
	}
	observationError := errors.New("interface inventory unavailable")
	state, missing, err := StartAddressReadiness(instance, func() ([]netip.Addr, error) {
		return nil, observationError
	})
	if !errors.Is(err, observationError) || state != "" || len(missing) != 0 {
		t.Fatalf("observation error fabricated wait: %s %v %v", state, missing, err)
	}
	_, missing, err = StartAddressReadiness(instance, func() ([]netip.Addr, error) { return nil, nil })
	if err != nil || !reflect.DeepEqual(missing, []string{"192.0.2.11", "192.0.2.12"}) {
		t.Fatalf("waiting addresses are not deterministic: %v %v", missing, err)
	}
}
