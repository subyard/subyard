package reconcileruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestServiceRecreationGuardUsesApprovedNetworkManagerReader(t *testing.T) {
	for _, test := range []struct {
		name, uid, approved, expired string
		wantFailure, wantSudo        bool
	}{
		{name: "approved operator", uid: "1000", approved: "1", wantSudo: true},
		{name: "unapproved operator", uid: "1000", wantFailure: true},
		{name: "expired approval", uid: "1000", approved: "1", expired: "1", wantFailure: true, wantSudo: true},
		{name: "root", uid: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			library, err := filepath.Abs("../../../scripts/lib-power.sh")
			if err != nil {
				t.Fatal(err)
			}
			write := func(name, script string) {
				t.Helper()
				testkit.WriteFile(t, filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+script), 0o700)
			}
			write("id", "printf '%s\\n' \"$MOCK_UID\"\n")
			write("sudo", `printf 'sudo\n' >> "$READ_LOG"
[ "$1" = -n ] && [ "$2" = -- ] || exit 90
[ "$MOCK_EXPIRED" != 1 ] || exit 1
shift 2
export MOCK_PRIVILEGED=1
exec "$@"
`)
			write("NetworkManager", `[ "$*" = --print-config ] || exit 91
[ "$MOCK_UID" = 0 ] || [ "${MOCK_PRIVILEGED:-0}" = 1 ] || exit 7
printf '[device-subyard]\nmatch-device=type:veth,driver:veth,interface-name:tap????????,interface-name:incusbr0\nmanaged=0\n'
`)
			write("systemctl", "printf 'active\\n'\n")
			write("nmcli", "printf 'incusbr0:unmanaged\\n'\n")
			// An independent route refusal must survive a failed privileged read.
			write("ip", "printf '%s\\n' \"$MOCK_ROUTES\"\n")
			testkit.WriteFile(t, filepath.Join(root, "scripts/install-power-reconciler.sh"), []byte(`#!/bin/bash
set -euo pipefail
[ "$*" = --print-network-manager-config ] || exit 92
. "$TEST_POWER_LIBRARY"
power_nm_binary() { printf '%s\n' "$TEST_NM_BINARY"; }
power_nm_print_config
`), 0o700)
			routes := `[]`
			if test.wantFailure {
				routes = `[{"dev":"incusbr0"}]`
			}
			log := filepath.Join(root, "reader.log")
			runtime := Runtime{RepositoryRoot: root, Environment: []string{
				"PATH=" + bin + ":/usr/bin:/bin", "MOCK_UID=" + test.uid,
				"SUBYARD_SUDO_PREAUTHORIZED=" + test.approved, "MOCK_EXPIRED=" + test.expired,
				"TEST_POWER_LIBRARY=" + library, "TEST_NM_BINARY=" + filepath.Join(bin, "NetworkManager"),
				"READ_LOG=" + log, "MOCK_ROUTES=" + routes,
			}}
			guard := runtime.serviceRecreationNetworkGuard()
			guard.Lookup = func(name string) (string, bool) { return filepath.Join(bin, name), true }
			guard.State = func() string { return "loaded" }
			err = guard.Check(context.Background(), []string{"incusbr0"})
			if test.wantFailure {
				if err == nil || !strings.Contains(err.Error(), "cannot read NetworkManager") || !strings.Contains(err.Error(), "unsafe host default route") {
					t.Fatalf("privileged read failure hid an independent refusal: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			payload, readErr := os.ReadFile(log)
			if test.wantSudo && (readErr != nil || string(payload) != "sudo\n") || !test.wantSudo && !os.IsNotExist(readErr) {
				t.Fatalf("unexpected privilege boundary: %q, %v", payload, readErr)
			}
		})
	}
}
