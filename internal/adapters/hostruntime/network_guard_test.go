package hostruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
		"run yard init",
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

// The shell rule in scripts/lib-power.sh reads the same vectors in tests/lib-power-network.sh.
func TestNetworkManagerGuardStateSharedVectors(t *testing.T) {
	vectors, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixtures", "nm-guard-state.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(vectors)), "\n") {
		fields := strings.Split(line, "|")
		if got := NetworkManagerGuardState(fields[0], fields[1], fields[2], fields[3], fields[4] == "1"); got != fields[5] {
			t.Errorf("%q: state %q, want %q", line, got, fields[5])
		}
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
	guard.State = func() string { return "unread-migration" }
	err := guard.Check(context.Background(), []string{"incusbr0"})
	if err == nil || !strings.Contains(err.Error(), "reboot the host once, then rerun yard start") {
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

func TestNetworkGuardRefusesAnUnreadGuard(t *testing.T) {
	guard := activeGuard(currentGuardConfig, "incusbr0:unmanaged\n", `[]`)
	guard.State = func() string { return "unread-init" }
	err := guard.Check(context.Background(), []string{"incusbr0"})
	if err == nil || !strings.Contains(err.Error(), "has not read the Subyard guard") ||
		!strings.Contains(err.Error(), "restart NetworkManager") {
		t.Fatalf("an unread guard did not refuse the start: %v", err)
	}
	guard.State = func() string { return "loaded-reboot" }
	if err := guard.Check(context.Background(), []string{"incusbr0"}); err != nil {
		t.Fatalf("a migrated guard waiting for the reboot blocked the start: %v", err)
	}
}

func TestNetworkManagerGuardCurrentTreatsADanglingGuardLinkAsReleased(t *testing.T) {
	directory := testkit.TempDir(t)
	conf := filepath.Join(directory, "zz-subyard-unmanaged.conf")
	if err := os.Symlink(filepath.Join(directory, "missing"), conf); err != nil {
		t.Fatal(err)
	}
	if current, err := NetworkManagerGuardCurrent(conf); err != nil || current {
		t.Fatalf("dangling guard link: current=%v err=%v", current, err)
	}
}

// Removing any one rendered entry fails the rule (the shell twin runs in tests/lib-power-network.sh).
func TestNetworkManagerGuardRuleRequiresEveryRenderedEntry(t *testing.T) {
	entries := append(slices.Clone(NetworkManagerGuardStaticEntries), "interface-name:incusbr0")
	for index := range entries {
		kept := slices.Delete(slices.Clone(entries), index, index+1)
		config := "[device-subyard]\nmatch-device=" + strings.Join(kept, ",") + "\nmanaged=0\n"
		if violations := networkManagerConfigViolations(config, []string{"incusbr0"}); len(violations) == 0 {
			t.Errorf("a guard without %s passed", entries[index])
		}
	}
}

func TestLocalNetworkManagerGuardStateNeverLoadsFromAnUnreadableRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	directory := testkit.TempDir(t)
	guard, boot, record := filepath.Join(directory, "zz.conf"), filepath.Join(directory, "boot_id"), filepath.Join(directory, "state")
	testkit.WriteFile(t, guard, []byte("[device-subyard]\n"), 0o644)
	testkit.WriteFile(t, boot, []byte("b1\n"), 0o644)
	fileID := networkManagerFileID(guard)
	testkit.WriteFile(t, record, []byte("b1 "+fileID+" nm1 held init\n"), 0o000)
	if got := networkManagerGuardStateAt(record, guard, boot, "nm1", func(string) bool { return true }); got != "unread-init" {
		t.Fatalf("an unreadable record gave %q", got)
	}
	if err := os.Chmod(record, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := networkManagerGuardStateAt(record, guard, boot, "nm1", func(string) bool { return false }); got != "loaded" {
		t.Fatalf("a readable held record gave %q", got)
	}
}
