package hostruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
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

const currentGuardConfig = "[keyfile]\nunmanaged-devices=except:type:wifi\n[device-subyard]\n" +
	"match-device=type:veth,driver:veth,interface-name:tap????????,interface-name:incusbr0\nmanaged=0\n"

func activeGuard(config, devices, routes string) NetworkGuard {
	return testNetworkGuard(map[string]bool{"systemctl": true, "NetworkManager": true, "nmcli": true, "ip": true},
		func(name string, arguments ...string) ([]byte, error) {
			switch name {
			case "systemctl":
				return []byte("active\n"), nil
			case "NetworkManager":
				return []byte(config), nil
			case "nmcli":
				return []byte(devices), nil
			case "ip":
				if len(arguments) > 1 && arguments[1] == "-4" {
					return []byte(routes), nil
				}
				return []byte(`[{}]`), nil
			}
			return nil, errors.New("unexpected command")
		})
}

func TestNetworkGuardValidatesEffectiveNetworkManagerConfig(t *testing.T) {
	guard := activeGuard(currentGuardConfig, "lo:connected (externally)\nincusbr0:unmanaged\n", `[]`)
	if err := guard.Check(context.Background(), []string{"incusbr0"}); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkGuardReportsConfigBridgeStateAndRouteTogether(t *testing.T) {
	config := strings.Replace(currentGuardConfig, ",interface-name:incusbr0", "", 1)
	guard := activeGuard(config, "incusbr0:connected (externally)\n", `[{"dev":"veth1234"}]`)
	err := guard.Check(context.Background(), []string{"incusbr0"})
	for _, want := range []string{
		"[device-subyard] match-device lacks interface-name:incusbr0",
		"NetworkManager manages yard bridge incusbr0",
		"then rerun yard start",
		`unsafe host default route uses "veth1234"`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q in %v", want, err)
		}
	}
}

// The shell rule in scripts/lib-power.sh runs the same vectors in tests/lib-power-network.sh.
func TestNetworkManagerConfigViolationsSharedVectors(t *testing.T) {
	root := filepath.Join("..", "..", "..", "tests", "fixtures", "nm-guard-vectors")
	cases, err := os.ReadDir(root)
	if err != nil || len(cases) == 0 {
		t.Fatalf("read shared vectors: %v", err)
	}
	for _, entry := range cases {
		t.Run(entry.Name(), func(t *testing.T) {
			read := func(name string) string {
				contents, err := os.ReadFile(filepath.Join(root, entry.Name(), name))
				if err != nil {
					t.Fatal(err)
				}
				return string(contents)
			}
			got := strings.Join(networkManagerConfigViolations(read("config"), strings.Fields(read("bridges"))), "\n")
			if want := strings.TrimSuffix(read("violations"), "\n"); got != want {
				t.Fatalf("violations:\n%s\nwant:\n%s", got, want)
			}
		})
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

func TestRouteDevicesAreSubyardsOwn(t *testing.T) {
	for device, unsafe := range map[string]bool{
		"incusbr0": true, "veth1234": true, "tap1234abcd": true,
		"tap0": false, "br-0123456789ab": false, "docker0": false, "virbr0": false, "vnet3": false, "enp5s0": false,
	} {
		if got := unsafeDeviceName(device, []string{"incusbr0"}); got != unsafe {
			t.Errorf("unsafeDeviceName(%q) = %v, want %v", device, got, unsafe)
		}
	}
}

func TestNetworkManagerRebootPendingFollowsTheBootAndNetworkManager(t *testing.T) {
	directory := testkit.TempDir(t)
	pending, boot := filepath.Join(directory, "pending"), filepath.Join(directory, "boot_id")
	invocation := "nm-1"
	current := func() string { return invocation }
	if NetworkManagerRebootPending(pending, boot, current) {
		t.Fatal("absent record reported a pending reboot")
	}
	testkit.WriteFile(t, boot, []byte("boot-a\n"), 0o644)
	testkit.WriteFile(t, pending, []byte("boot-a nm-1\n"), 0o644)
	if !NetworkManagerRebootPending(pending, boot, current) {
		t.Fatal("same boot and NetworkManager did not report the pending reboot")
	}
	invocation = "nm-2"
	if NetworkManagerRebootPending(pending, boot, current) {
		t.Fatal("a NetworkManager restart did not apply the migrated guard")
	}
	testkit.WriteFile(t, pending, []byte("boot-a unknown\n"), 0o644)
	if !NetworkManagerRebootPending(pending, boot, current) {
		t.Fatal("an inactive NetworkManager at migration did not keep the boot ID rule")
	}
	testkit.WriteFile(t, boot, []byte("boot-b\n"), 0o644)
	if NetworkManagerRebootPending(pending, boot, current) {
		t.Fatal("a new boot still reported the pending reboot")
	}
}

// The shell classifier in scripts/lib/host.sh reads the same fixtures in tests/lib-power-network.sh.
func TestNetworkManagerGuardFormatSharedFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "..", "tests", "fixtures", "nm-guard-formats")
	expected, err := os.ReadFile(filepath.Join(root, "expected"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(expected)), "\n") {
		name, want, _ := strings.Cut(line, " ")
		directory := testkit.TempDir(t)
		conf := filepath.Join(directory, "zz-subyard-unmanaged.conf")
		contents, err := os.ReadFile(filepath.Join(root, name+".conf"))
		if err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, conf, contents, 0o644)
		got, err := NetworkManagerGuardCurrent(conf)
		if err != nil || got != (want == "current") {
			t.Errorf("%s: current=%v err=%v, want %s", name, got, err, want)
		}
	}
}

func TestNetworkGuardNamesThePendingReboot(t *testing.T) {
	config := strings.Replace(currentGuardConfig, ",interface-name:incusbr0", "", 1)
	guard := activeGuard(config, "incusbr0:unmanaged\n", `[]`)
	guard.RebootPending = func() bool { return true }
	err := guard.Check(context.Background(), []string{"incusbr0"})
	if err == nil || !strings.Contains(err.Error(), "waits for a host reboot: reboot the host once") {
		t.Fatalf("expected the pending reboot in %v", err)
	}
}

func TestNetworkGuardRefusesAFailedDeviceRead(t *testing.T) {
	guard := activeGuard(currentGuardConfig, "", `[]`)
	err := guard.Check(context.Background(), []string{"incusbr0"})
	if err == nil || !strings.Contains(err.Error(), "cannot read NetworkManager's devices") {
		t.Fatalf("an empty device read was accepted: %v", err)
	}
}

func TestNetworkManagerGuardCurrentRejectsReleasedForms(t *testing.T) {
	current := "# Subyard\n[device-subyard]\nmatch-device=type:veth,driver:veth,interface-name:incusbr0\nmanaged=0\n"
	released := "[main]\nno-auto-default=type:veth\n\n[keyfile]\nunmanaged-devices=type:veth\n\n" +
		"[device-subyard]\nmatch-device=type:veth\nmanaged=0\n"
	for name, test := range map[string]struct {
		guard, legacy string
		want          bool
	}{
		"absent": {want: true}, "current": {guard: current, want: true},
		"released": {guard: released}, "appending key": {guard: "[keyfile]\nunmanaged-devices+=type:veth\n"},
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

func TestNetworkManagerGuardCurrentCountsADanglingLegacyLink(t *testing.T) {
	directory := testkit.TempDir(t)
	if err := os.Symlink(filepath.Join(directory, "missing"), filepath.Join(directory, "99-subyard-unmanaged.conf")); err != nil {
		t.Fatal(err)
	}
	if current, err := NetworkManagerGuardCurrent(filepath.Join(directory, "zz-subyard-unmanaged.conf")); err != nil || current {
		t.Fatalf("dangling legacy link: current=%v err=%v", current, err)
	}
}
