package hostruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func testNetworkGuard(
	binaries map[string]bool,
	run func(string, ...string) ([]byte, error),
) NetworkGuard {
	return NetworkGuard{
		Lookup: func(name string) (string, bool) {
			if binaries[name] {
				return "/test/" + name, true
			}
			return "", false
		},
		Run: func(_ context.Context, name string, arguments ...string) ([]byte, error) {
			return run(strings.TrimPrefix(name, "/test/"), arguments...)
		},
	}
}

func TestNetworkGuardAcceptsSafeRoutesWithoutNetworkManager(t *testing.T) {
	guard := testNetworkGuard(map[string]bool{"systemctl": true, "ip": true},
		func(name string, arguments ...string) ([]byte, error) {
			switch fmt.Sprint(append([]string{name}, arguments...)) {
			case "[systemctl is-active NetworkManager]":
				return []byte("inactive\n"), errors.New("inactive")
			case "[ip -j -4 route show default]":
				return []byte(`[{"dev":"enp1s0"}]`), nil
			case "[ip -j -d link show dev enp1s0]":
				return []byte(`[{}]`), nil
			default:
				return nil, errors.New("unexpected command")
			}
		})
	if err := guard.Check(context.Background(), []string{"incusbr0"}); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkGuardValidatesEffectiveNetworkManagerConfig(t *testing.T) {
	guard := testNetworkGuard(map[string]bool{"systemctl": true, "NetworkManager": true, "ip": true},
		func(name string, arguments ...string) ([]byte, error) {
			switch name {
			case "systemctl":
				return []byte("active\n"), nil
			case "NetworkManager":
				// "+=" values merged onto an admin list are comma-joined.
				return []byte("[main]\nno-auto-default=interface-name:enp5s0\n[keyfile]\n" +
					"unmanaged-devices=interface-name:enp5s0,type:veth,driver:veth,interface-name:incusbr0\n"), nil
			case "ip":
				if len(arguments) > 1 && arguments[1] == "-4" {
					return []byte(`[]`), nil
				}
			}
			return nil, errors.New("unexpected command")
		})
	if err := guard.Check(context.Background(), []string{"incusbr0"}); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkGuardRejectsIncompleteNetworkManagerConfig(t *testing.T) {
	guard := testNetworkGuard(map[string]bool{"systemctl": true, "NetworkManager": true, "ip": true},
		func(name string, _ ...string) ([]byte, error) {
			if name == "systemctl" {
				return []byte("active\n"), nil
			}
			if name == "NetworkManager" {
				return []byte("[keyfile]\nunmanaged-devices=type:veth;driver:veth\n"), nil
			}
			return []byte(`[]`), nil
		})
	err := guard.Check(context.Background(), []string{"incusbr0"})
	if err == nil || !strings.Contains(err.Error(), "lacks interface-name:incusbr0;") {
		t.Fatalf("expected missing bridge guard, got %v", err)
	}
}

func TestNetworkGuardRejectsUnsafeDefaultRoute(t *testing.T) {
	guard := testNetworkGuard(map[string]bool{"systemctl": true, "ip": true},
		func(name string, arguments ...string) ([]byte, error) {
			if name == "systemctl" {
				return []byte("inactive\n"), nil
			}
			if len(arguments) > 1 && arguments[1] == "-4" {
				return []byte(`[{"dev":"veth1234"}]`), nil
			}
			return []byte(`[{}]`), nil
		})
	err := guard.Check(context.Background(), []string{"incusbr0"})
	if err == nil || !strings.Contains(err.Error(), "unsafe host default route") {
		t.Fatalf("expected unsafe route error, got %v", err)
	}
}

func TestNetworkGuardFailsClosedWhenNetworkManagerStateIsUnknown(t *testing.T) {
	guard := testNetworkGuard(map[string]bool{"NetworkManager": true, "ip": true},
		func(string, ...string) ([]byte, error) { return nil, nil })
	err := guard.Check(context.Background(), []string{"incusbr0"})
	if err == nil || !strings.Contains(err.Error(), "cannot inspect NetworkManager") {
		t.Fatalf("expected NetworkManager state error, got %v", err)
	}
}

func TestNetworkGuardRejectsUnsafeBridgeName(t *testing.T) {
	guard := testNetworkGuard(nil, func(string, ...string) ([]byte, error) { return nil, nil })
	if err := guard.Check(context.Background(), []string{"incusbr0;reboot"}); err == nil {
		t.Fatal("unsafe bridge name accepted")
	}
}

func TestNetworkManagerConfigReportsEveryMissingEntryAndTheNextStep(t *testing.T) {
	// A later plain "=" replaced the Subyard list; a key outside [keyfile] does not count.
	config := "[main]\nunmanaged-devices=type:veth;driver:veth;interface-name:incusbr0\n" +
		"[keyfile]\nunmanaged-devices=interface-name:enp5s0\n"
	err := validateNetworkManagerConfig(config, []string{"incusbr0", "nestedbr0"})
	want := "lacks type:veth,driver:veth,interface-name:incusbr0,interface-name:nestedbr0;"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "use unmanaged-devices+= there") {
		t.Fatalf("expected every missing entry and the += next step, got %v", err)
	}
}

func TestDockerBridgeRouteNamesExcludeAdminBridges(t *testing.T) {
	for device, unsafe := range map[string]bool{
		"br-0123456789ab": true, "br-lan": false, "br-0123456789AB": false, "br-0123456789abc": false, "incusbr0": true,
	} {
		if got := unsafeDeviceName(device, []string{"incusbr0"}); got != unsafe {
			t.Errorf("unsafeDeviceName(%q) = %v, want %v", device, got, unsafe)
		}
	}
}

func TestNetworkManagerGuardCurrentRejectsReleasedForms(t *testing.T) {
	current := "# Subyard\n[keyfile]\nunmanaged-devices+=type:veth;driver:veth;interface-name:incusbr0\n"
	released := "[main]\nno-auto-default=type:veth\n\n[keyfile]\nunmanaged-devices=type:veth\n\n" +
		"[device-subyard]\nmatch-device=type:veth\nmanaged=0\n"
	for name, test := range map[string]struct {
		guard, legacy string
		want          bool
	}{
		"absent": {want: true}, "current": {guard: current, want: true},
		"released": {guard: released}, "plain key": {guard: "[keyfile]\nunmanaged-devices=type:veth\n"},
		"legacy":      {guard: current, legacy: "[keyfile]\nunmanaged-devices=type:veth\n"},
		"legacy only": {legacy: "[keyfile]\nunmanaged-devices=type:veth\n"},
	} {
		t.Run(name, func(t *testing.T) {
			directory := testkit.TempDir(t)
			conf := filepath.Join(directory, "zz-subyard-unmanaged.conf")
			for path, contents := range map[string]string{conf: test.guard, filepath.Join(directory, "99-subyard-unmanaged.conf"): test.legacy} {
				if contents != "" {
					testkit.WriteFile(t, path, []byte(contents), 0o644)
				}
			}
			got, err := NetworkManagerGuardCurrent(conf)
			if err != nil || got != test.want {
				t.Fatalf("NetworkManagerGuardCurrent = %v, %v; want %v", got, err, test.want)
			}
		})
	}
}
