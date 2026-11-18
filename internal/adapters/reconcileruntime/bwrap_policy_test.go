package reconcileruntime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestBubblewrapPolicyResumesPendingRecordWithoutDirectory(t *testing.T) {
	root := testkit.TempDir(t)
	for _, dir := range []string{"records", "systemd/wants", "apparmor/policy/profiles", "run"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	const receipt = "records/bwrap-apparmor-ownership"
	testkit.WriteFile(t, filepath.Join(root, receipt+".pending"), []byte(fmt.Sprintf("%064d  unpriv_bwrap\n%064d  bwrap\n", 0, 0)), 0644)
	for _, path := range []string{"apparmor/.ns_level", "restricted", "apparmor/.load"} {
		testkit.WriteFile(t, filepath.Join(root, path), []byte("1\n"), 0644)
	}
	testkit.WriteFile(t, filepath.Join(root, "bwrap"), nil, 0755)
	// Use ordinary files for the AppArmor and systemd endpoints. Keep the real
	// ownership checks, pending cleanup, publication and post-publication check.
	const lock = "if [ \"$mode\" != observe ]; then\n  exec"
	if !strings.Contains(bubblewrapPolicy, lock) {
		t.Fatal("fixture cannot intercept the policy's system commands")
	}
	script := strings.NewReplacer(
		"policy_parent=/opt/subyard", "fixture=$7\npolicy_parent=$fixture/payload",
		"record_parent=/var/lib/subyard", "record_parent=$fixture/records",
		"/etc/systemd/system/multi-user.target.wants", "$fixture/systemd/wants",
		"/etc/systemd/system", "$fixture/systemd",
		"/opt/subyard/codex", "$fixture/payload/codex",
		"/etc/apparmor.d/", "$fixture/admin/",
		"/sys/kernel/security/apparmor", "$fixture/apparmor",
		"/proc/sys/kernel/apparmor_restrict_unprivileged_userns", `"$fixture/restricted"`,
		"/run/subyard-bwrap-apparmor.lock", `"$fixture/run/lock"`,
		"/usr/bin/bwrap", `"$fixture/bwrap"`,
		`"0:0:$2"`, fmt.Sprintf(`"%d:%d:$2"`, os.Getuid(), os.Getgid()),
		lock, `systemctl() {
  case "$1" in
    show) printf '%s\n' "$fixture/systemd" ;;
    enable) ln -s "$unit" "$link" ;;
    daemon-reload) : ;;
    *) return 1 ;;
  esac
}
compile_policy() {
  printf 'profile bwrap\n' > "$1/bwrap"
  printf 'profile unpriv_bwrap\n' > "$1/unpriv_bwrap"
}
load_missing() {
  cmp -s "$record" <(cd "$policy" && sha256sum "${names[@]}")
  touch "$fixture/loaded"
}
runuser() { test -f "$fixture/loaded"; }

if [ "$mode" != observe ]; then
  exec`,
	).Replace(bubblewrapPolicy)
	command := exec.Command("bash", "-c", script, "subyard", "apply", "on", "dev", "0", "container", "1", root)
	command.Env = append(os.Environ(), "BASH_ENV=/dev/null")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("first retry failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(root, receipt+".pending")); !os.IsNotExist(err) {
		t.Fatalf("pending record after retry: %v", err)
	}
	unit := filepath.Join(root, "systemd", "subyard-bwrap-apparmor.service")
	if link, err := os.Readlink(filepath.Join(root, "systemd", "wants", filepath.Base(unit))); err != nil || link != unit {
		t.Fatalf("boot registration after retry: %q, %v", link, err)
	}
}

func TestBubblewrapPolicyExtractsDistributionParser(t *testing.T) {
	functions, _, found := strings.Cut(bubblewrapPolicy, "\ncompile_policy()")
	if !found {
		t.Fatal("policy compiler definition is missing")
	}
	for _, tc := range []struct{ name, path string }{
		{"Ubuntu 24.04", "sbin/apparmor_parser"},
		{"Debian 13", "usr/sbin/apparmor_parser"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := testkit.TempDir(t)
			root := filepath.Join(dir, "package")
			for _, subdir := range []string{"DEBIAN", filepath.Dir(tc.path)} {
				path := filepath.Join(root, subdir)
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, filepath.Join(root, "DEBIAN/control"),
				[]byte("Package: apparmor\nVersion: 1\nArchitecture: all\nMaintainer: Test <test@example.invalid>\nDescription: policy fixture\n"), 0o644)
			const parser = "#!/bin/sh\nexit 0\n"
			testkit.WriteFile(t, filepath.Join(root, tc.path), []byte(parser), 0o755)
			archive := filepath.Join(dir, "apparmor.deb")
			if output, err := exec.Command("dpkg-deb", "--build", "--root-owner-group", root, archive).CombinedOutput(); err != nil {
				t.Fatalf("build package: %v\n%s", err, output)
			}
			output, err := exec.Command("bash", "-c", functions+"\nextract_parser \"$2\"", "subyard", "observe", archive).CombinedOutput()
			if err != nil || string(output) != parser {
				t.Fatalf("extract %s: %v\n%s", tc.path, err, output)
			}
		})
	}
}
